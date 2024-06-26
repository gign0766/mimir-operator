package mimirrules

import (
	"context"
	"fmt"
	"slices"

	prometheus "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	domain "github.com/AmiditeX/mimir-operator/api/v1alpha1"
	"github.com/AmiditeX/mimir-operator/internal/controller/mimirapi"
	"github.com/AmiditeX/mimir-operator/internal/utils"
)

const (
	mimirFinalizer = "mimir.randgen.xyz/finalizer"
	temporaryFiles = "/tmp/"
)

// MimirRulesReconciler reconciles a MimirRules object
type MimirRulesReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=mimir.randgen.xyz,resources=mimirrules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=mimir.randgen.xyz,resources=mimirrules/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=mimir.randgen.xyz,resources=mimirrules/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *MimirRulesReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the MimirRules
	mr := &domain.MimirRules{}
	err := r.Get(ctx, req.NamespacedName, mr)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	log.FromContext(ctx).Info("Running reconcile on MimirRules")

	mc, err := r.createMimirClient(ctx, mr)
	if err != nil {
		// Update status with an error if we can't create a client for Mimir Api
		return ctrl.Result{}, r.setStatus(ctx, mr, err)
	}

	// Examine DeletionTimestamp to determine if object is under deletion
	if mr.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer
		if !controllerutil.ContainsFinalizer(mr, mimirFinalizer) {
			controllerutil.AddFinalizer(mr, mimirFinalizer)
			if err := r.Update(ctx, mr); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(mr, mimirFinalizer) {
			if err := r.handleDeletion(ctx, mr, mc); err != nil {
				// Status is set only on failure to delete (the status is going to be deleted anyway if it succeeds)
				return ctrl.Result{}, r.setStatus(ctx, mr, err)
			}

			// Remove our finalizer from the list and update it
			controllerutil.RemoveFinalizer(mr, mimirFinalizer)
			return ctrl.Result{}, r.Update(ctx, mr)
		}
	}

	return ctrl.Result{}, r.handleCreationAndChanges(ctx, mr, mc)
}

func (r *MimirRulesReconciler) createMimirClient(ctx context.Context, mr *domain.MimirRules) (*mimirapi.MimirClient, error) {
	auth, err := utils.ExtractAuth(ctx, r.Client, mr.Spec.Auth, mr.ObjectMeta.Namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to extract authentication settings: %w", err)
	}

	c, err := mimirapi.New(mimirapi.Config{
		User:      auth.Username,
		Key:       auth.Key,
		AuthToken: auth.Token,
		Address:   mr.Spec.URL,
		ID:        mr.Spec.ID,
	})

	if err != nil {
		return nil, fmt.Errorf("failed to create mimir client: %w", err)
	}

	return c, nil
}

// handleCreationAndChanges handles reconciliation of MimirRules for events that are not a deletion
// This means that this function will be called for any modification in a MimirRules or for
// any creation of a new MimirRules in the API. It is also called periodically for scheduled
// reconciliation and at the startup of the controller.
func (r *MimirRulesReconciler) handleCreationAndChanges(ctx context.Context, mr *domain.MimirRules, mc *mimirapi.MimirClient) error {
	reconciliationError := r.reconcileRules(ctx, mr, mc)
	if err := r.setStatus(ctx, mr, reconciliationError); err != nil {
		return err
	}

	log.FromContext(ctx).Info("MimirRules correctly synchronized")

	return nil
}

// handleDeletion handles cleaning up after the deletion of a MimirRules
func (r *MimirRulesReconciler) handleDeletion(ctx context.Context, mr *domain.MimirRules, mc *mimirapi.MimirClient) error {
	log.FromContext(ctx).Info("Running reconciliation on deletion of a MimirRules")
	return r.deleteRulesForTenant(ctx, mr, mc)
}

// reconcileRules ensures Mimir is synced with the PrometheusRules associated with a MimirRules
func (r *MimirRulesReconciler) reconcileRules(ctx context.Context, mr *domain.MimirRules, mc *mimirapi.MimirClient) error {
	log.FromContext(ctx).Info("Running reconciliation of the rules")

	return r.syncRulesToRuler(ctx, mc, mr)
}

// reconcileOnPrometheusRuleChange sends a reconcile request to EVERY MimirRule on the cluster
// This is done to retrigger the synchronization of MimirRules if new PrometheusRules have been added
// or if some PrometheusRules have changed their definition
func (r *MimirRulesReconciler) reconcileOnPrometheusRuleChange(ctx context.Context, rule client.Object) []reconcile.Request {
	allMimirRules := &domain.MimirRulesList{}
	err := r.List(context.Background(), allMimirRules)
	if err != nil {
		log.FromContext(context.TODO()).Error(err, "failed to list all MimirRules after a PrometheusRule change")
		return []reconcile.Request{}
	}

	// Check for existance of the rule
	pr := &prometheus.PrometheusRule{}
	err = r.Get(ctx, types.NamespacedName{Namespace: rule.GetNamespace(), Name: rule.GetName()}, pr)

	requests := make([]reconcile.Request, 0)
	// If Prometheus Rule is not found then it must have been deleted
	if err != nil && errors.IsNotFound(err) {
		// We update every MimirRule concerned by the delete Prometheus Rule
		for _, item := range allMimirRules.Items {
			namespaceAndName := rule.GetNamespace() + "_" + rule.GetName()

			if slices.Contains(item.Status.RefRules, namespaceAndName) {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      item.GetName(),
						Namespace: item.GetNamespace(),
					}})
			}
		}
	} else { // Prometheus rule have been updated or created, update only concerned rules
		for _, item := range allMimirRules.Items {
			promRulesList, _ := r.findPrometheusRulesFromLabels(ctx, item.Spec.Rules.Selectors)
			namespaceAndName := rule.GetNamespace() + "_" + rule.GetName()
			// If the updated/created rule affect the MimirRule then we request a reconcialition on it
			// We check if the MimirRule match with the Prometheus Rule
			// But also if the MimirRule previously match the Prometheus Rule (for example if the group change for the rule and not matching anymore)
			if contains(promRulesList, rule.GetNamespace(), rule.GetName()) || slices.Contains(item.Status.RefRules, namespaceAndName) {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      item.GetName(),
						Namespace: item.GetNamespace(),
					}})
			}
		}
	}

	return requests
}

// setStatus updates the status of MimirRules after reconciliation
// If err is not nil, the error field is populated with the error and the status is set as "Failed"
// Otherwise, status is set as "Synced"
func (r *MimirRulesReconciler) setStatus(ctx context.Context, mr *domain.MimirRules, err error) error {
	if err != nil {
		mr.Status.Status = "Failed"
		mr.Status.Error = err.Error()

		// Also log the error in the controller for clarity
		log.FromContext(ctx).Error(err, "Failed to reconcile MimirRules")
	} else {
		mr.Status.Status = "Synced"
		mr.Status.Error = ""
	}

	return r.Status().Update(context.Background(), mr)
}

// SetupWithManager sets up the controller with the Manager.
func (r *MimirRulesReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&domain.MimirRules{}).
		Watches( // Setup WATCH on PrometheusRules to dynamically reload MimirRules into the MimirRuler if a selected rule has been changed
			&prometheus.PrometheusRule{},
			handler.EnqueueRequestsFromMapFunc(r.reconcileOnPrometheusRuleChange)).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 32,
		}).
		Complete(r)
}

// Check if the prometheus rule list has a matching rule with namespace and name combinaison
func contains(pL *prometheus.PrometheusRuleList, namespace, name string) bool {
	for _, p := range pL.Items {
		if p.Name == name && p.Namespace == namespace {
			return true
		}
	}
	return false
}
