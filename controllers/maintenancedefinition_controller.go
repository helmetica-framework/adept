package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	batchv1ac "k8s.io/client-go/applyconfigurations/batch/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
	ritualsacv1 "github.com/helmetica-framework/adept/applyconfiguration/api/v1"
	"github.com/helmetica-framework/adept/schedule"
)

// fieldOwner is the server-side-apply field manager of the CronJob and the
// status adept writes.
const fieldOwner = client.FieldOwner("adept:maintenancedefinition")

// +kubebuilder:rbac:groups=rituals.helmetica.io,resources=maintenancedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=rituals.helmetica.io,resources=maintenancedefinitions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=create;get;list;watch;update;patch;delete

// MaintenanceDefinitionManager reconciles MaintenanceDefinition objects into
// the CronJob that fires their ritual.
type MaintenanceDefinitionManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Log      logr.Logger
}

// Reconcile drives a MaintenanceDefinition's status to reflect desiredState,
// which is where the CronJob behind it is written. A resolution failure is
// reported in the status and returned, so the workqueue retries it: the chart
// may render this object before the window exists.
func (r *MaintenanceDefinitionManager) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("maintenancedefinition", req.NamespacedName)

	md := &ritualsv1.MaintenanceDefinition{}
	err := r.Get(ctx, req.NamespacedName, md)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The CronJob is owned by the definition, so it goes with it.
			log.V(1).Info("maintenance definition is gone, nothing to do")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !md.GetDeletionTimestamp().IsZero() {
		log.V(1).Info("maintenance definition is being deleted, nothing to do")
		return ctrl.Result{}, nil
	}

	want, resolveErr := r.desiredState(ctx, md)
	if resolveErr != nil {
		want.Message = resolveErr.Error()
	}
	want.ObservedGeneration = md.Generation

	if md.Status != want {
		// Only on a change, so a backing-off retry does not spam events.
		if want.Message != "" && want.Message != md.Status.Message {
			r.Recorder.Eventf(md, nil, corev1.EventTypeWarning, "ScheduleResolveFailed", "Schedule", "%s", want.Message)
		}
		if want.Schedule != md.Status.Schedule {
			log.Info("schedule changed", "from", md.Status.Schedule, "to", want.Schedule)
		}

		status := ritualsacv1.MaintenanceDefinition(md.Name, md.Namespace).
			WithStatus(ritualsacv1.MaintenanceDefinitionStatus().
				WithSchedule(want.Schedule).
				WithCronJobName(want.CronJobName).
				WithObservedGeneration(want.ObservedGeneration).
				WithMessage(want.Message))

		if err := r.Status().Apply(ctx, status, fieldOwner, client.ForceOwnership); err != nil {
			return ctrl.Result{}, fmt.Errorf("applying maintenance definition status: %w", err)
		}
	}

	return ctrl.Result{}, resolveErr
}

// desiredState resolves the window and the ritual, applies the CronJob and
// reports what the instance ended up on. A failure to resolve any of them
// returns the message to report along with the error. A ritual that has since
// been deleted is such a failure, so its CronJob is left in place: the schedule
// stays visible and keeps its job history until the ritual comes back.
func (r *MaintenanceDefinitionManager) desiredState(ctx context.Context, md *ritualsv1.MaintenanceDefinition) (ritualsv1.MaintenanceDefinitionStatus, error) {
	ad := &ritualsv1.Definition{}

	err := r.Get(ctx, client.ObjectKey{Name: md.Spec.Ritual, Namespace: md.GetNamespace()}, ad)
	if err != nil {
		return ritualsv1.MaintenanceDefinitionStatus{}, fmt.Errorf("getting ritual %q: %w", md.Spec.Ritual, err)
	}

	wl := &ritualsv1.MaintenanceWindowList{}

	err = r.List(ctx, wl)
	if err != nil {
		return ritualsv1.MaintenanceDefinitionStatus{}, fmt.Errorf("listing maintenance windows: %w", err)
	}

	var window ritualsv1.MaintenanceWindow
	for _, w := range wl.Items {
		if md.Spec.Window == "" && w.Spec.Default {
			window = w
		}
		if md.Spec.Window == w.GetName() {
			window = w
		}
	}

	if window.Name == "" {
		if md.Spec.Window == "" {
			return ritualsv1.MaintenanceDefinitionStatus{}, fmt.Errorf("no maintenance window is marked as the default")
		}
		return ritualsv1.MaintenanceDefinitionStatus{}, fmt.Errorf("no maintenance window %q", md.Spec.Window)
	}

	cron, tz, err := schedule.CronSchedule(window.Spec, fmt.Sprintf("%s/%s", md.GetNamespace(), md.GetName()))
	if err != nil {
		return ritualsv1.MaintenanceDefinitionStatus{}, fmt.Errorf("resolving cron schedule: %w", err)
	}

	err = r.applyCronJob(ctx, md, ad, cron, tz)
	if err != nil {
		return ritualsv1.MaintenanceDefinitionStatus{}, fmt.Errorf("applying cronjob: %w", err)
	}

	return ritualsv1.MaintenanceDefinitionStatus{Schedule: cron, CronJobName: md.GetName()}, nil
}

// applyCronJob server-side-applies the CronJob the MaintenanceDefinition owns.
// Only the fields adept decides are set here; the job template is the
// Definition's, read into its apply configuration rather than converted from
// the whole object, which would claim every zero value as deliberate.
func (r *MaintenanceDefinitionManager) applyCronJob(ctx context.Context, md *ritualsv1.MaintenanceDefinition, def *ritualsv1.Definition, cronSchedule, timeZone string) error {
	owner, err := controllerRef(md, r.Scheme)
	if err != nil {
		return fmt.Errorf("building owner reference: %w", err)
	}

	raw, err := json.Marshal(def.Spec.JobTemplate)
	if err != nil {
		return fmt.Errorf("marshalling the job template: %w", err)
	}
	template := batchv1ac.JobTemplateSpec()
	if err := json.Unmarshal(raw, template); err != nil {
		return fmt.Errorf("reading the job template: %w", err)
	}

	cronJob := batchv1ac.CronJob(md.GetName(), md.GetNamespace()).
		WithOwnerReferences(owner).
		WithSpec(batchv1ac.CronJobSpec().
			WithSchedule(cronSchedule).
			WithTimeZone(timeZone).
			WithSuspend(md.Spec.Suspend).
			WithJobTemplate(template))

	return r.Apply(ctx, cronJob, fieldOwner, client.ForceOwnership)
}

// controllerRef builds the owner reference an apply configuration needs.
// controllerutil.SetControllerReference takes a client.Object and cannot set
// one.
func controllerRef(owner client.Object, scheme *runtime.Scheme) (*metav1ac.OwnerReferenceApplyConfiguration, error) {
	gvk, err := apiutil.GVKForObject(owner, scheme)
	if err != nil {
		return nil, err
	}

	return metav1ac.OwnerReference().
		WithAPIVersion(gvk.GroupVersion().String()).
		WithKind(gvk.Kind).
		WithName(owner.GetName()).
		WithUID(owner.GetUID()).
		WithController(true).
		WithBlockOwnerDeletion(true), nil
}

// MaintenanceWindowMapFunc maps a window to the MaintenanceDefinitions using
// it, by name and, when it is the default, by empty spec.window. Windows are
// cluster-scoped, so this lists across namespaces.
func (r *MaintenanceDefinitionManager) MaintenanceWindowMapFunc(ctx context.Context, o client.Object) []ctrl.Request {
	window, ok := o.(*ritualsv1.MaintenanceWindow)
	if !ok {
		return nil
	}

	list := &ritualsv1.MaintenanceDefinitionList{}
	if err := r.List(ctx, list); err != nil {
		r.Log.Error(err, "listing maintenance definitions for a window", "window", window.GetName())
		return nil
	}

	var requests []ctrl.Request
	for i := range list.Items {
		md := &list.Items[i]
		if md.Spec.Window == window.GetName() || (md.Spec.Window == "" && window.Spec.Default) {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(md)})
		}
	}

	return requests
}

// DefinitionMapFunc maps a Definition to the MaintenanceDefinitions in its
// namespace naming it, so repairing a ritual heals its instances.
func (r *MaintenanceDefinitionManager) DefinitionMapFunc(ctx context.Context, o client.Object) []ctrl.Request {
	list := &ritualsv1.MaintenanceDefinitionList{}
	if err := r.List(ctx, list, client.InNamespace(o.GetNamespace())); err != nil {
		r.Log.Error(err, "listing maintenance definitions for a ritual", "definition", client.ObjectKeyFromObject(o))
		return nil
	}

	var requests []ctrl.Request
	for i := range list.Items {
		md := &list.Items[i]
		if md.Spec.Ritual == o.GetName() {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(md)})
		}
	}

	return requests
}

// SetupWithManager wires the controller: watch MaintenanceDefinition, the
// CronJobs it owns, and the windows and Definitions they resolve through.
func (r *MaintenanceDefinitionManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&ritualsv1.MaintenanceDefinition{}).
		Owns(&batchv1.CronJob{}).
		Watches(&ritualsv1.MaintenanceWindow{}, handler.EnqueueRequestsFromMapFunc(r.MaintenanceWindowMapFunc)).
		Watches(&ritualsv1.Definition{}, handler.EnqueueRequestsFromMapFunc(r.DefinitionMapFunc)).
		Complete(r)
}
