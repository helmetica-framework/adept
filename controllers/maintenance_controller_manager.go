package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
const fieldOwner = client.FieldOwner("adept:maintenance")

// +kubebuilder:rbac:groups=rituals.helmetica.io,resources=maintenances,verbs=get;list;watch
// +kubebuilder:rbac:groups=rituals.helmetica.io,resources=maintenances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=create;get;list;watch;update;patch;delete

// MaintenanceManager reconciles Maintenance objects into the CronJob that
// fires their ritual.
type MaintenanceManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Log      logr.Logger
}

// Reconcile drives a Maintenance's status to reflect desiredState, which is
// where the CronJob behind it is written. A resolution failure is
// reported in the status and returned, so the workqueue retries it: the chart
// may render this object before the window exists.
func (r *MaintenanceManager) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("maintenance", req.NamespacedName)

	md := &ritualsv1.Maintenance{}
	err := r.Get(ctx, req.NamespacedName, md)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The CronJob is owned by the maintenance, so it goes with it.
			log.V(1).Info("maintenance is gone, nothing to do")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !md.GetDeletionTimestamp().IsZero() {
		log.V(1).Info("maintenance is being deleted, nothing to do")
		return ctrl.Result{}, nil
	}

	want, resolveErr := r.desiredState(ctx, md)
	if resolveErr != nil {
		want.Message = resolveErr.Error()
	}
	want.ObservedGeneration = md.Generation
	// VersionManager owns this one. Carrying it through keeps the comparison
	// below judging only the fields this controller writes.
	want.VersionUpdatedFor = md.Status.VersionUpdatedFor

	if md.Status != want {
		// Only on a change, so a backing-off retry does not spam events.
		if resolveErr != nil && want.Message != md.Status.Message {
			r.Recorder.Eventf(md, nil, corev1.EventTypeWarning, "ScheduleResolveFailed", "Schedule", "%s", want.Message)
		}
		if want.Schedule != md.Status.Schedule {
			log.Info("schedule changed", "from", md.Status.Schedule, "to", want.Schedule)
		}

		status := ritualsacv1.Maintenance(md.Name, md.Namespace).
			WithStatus(ritualsacv1.MaintenanceStatus().
				WithSchedule(want.Schedule).
				WithCronJobName(want.CronJobName).
				WithObservedGeneration(want.ObservedGeneration).
				WithMessage(want.Message))

		if err := r.Status().Apply(ctx, status, fieldOwner, client.ForceOwnership); err != nil {
			return ctrl.Result{}, fmt.Errorf("applying maintenance status: %w", err)
		}
	}

	return ctrl.Result{}, resolveErr
}

// desiredState resolves the window, then the ritual, applies the CronJob and
// reports what the instance ended up on. The window is required: without one
// there is no schedule, and the version bump has nothing to key off either, so
// failing to resolve it returns the message to report along with the error.
//
// The ritual is not required. A reagent may ship no maintenance Definition and
// still want its version moved on the window's schedule, so a missing one
// publishes the schedule, removes the CronJob and reports the reason without an
// error.
func (r *MaintenanceManager) desiredState(ctx context.Context, md *ritualsv1.Maintenance) (ritualsv1.MaintenanceStatus, error) {
	window, err := resolveWindow(ctx, r.Client, md)
	if err != nil {
		return ritualsv1.MaintenanceStatus{}, err
	}

	cron, tz, err := schedule.CronSchedule(window.Spec, spreadIdentity(md))
	if err != nil {
		return ritualsv1.MaintenanceStatus{}, fmt.Errorf("resolving cron schedule: %w", err)
	}

	ad := &ritualsv1.Definition{}

	err = r.Get(ctx, client.ObjectKey{Name: md.Spec.Ritual, Namespace: md.GetNamespace()}, ad)
	if err != nil && !apierrors.IsNotFound(err) {
		return ritualsv1.MaintenanceStatus{}, fmt.Errorf("getting ritual %q: %w", md.Spec.Ritual, err)
	}

	if err != nil && apierrors.IsNotFound(err) {
		if err := r.deleteCronJob(ctx, md); err != nil {
			return ritualsv1.MaintenanceStatus{}, err
		}
		return ritualsv1.MaintenanceStatus{
			Schedule: cron,
			Message:  fmt.Sprintf("no Definition %q in this namespace; version bumping only", md.Spec.Ritual),
		}, nil
	}

	err = r.applyCronJob(ctx, md, ad, cron, tz)
	if err != nil {
		return ritualsv1.MaintenanceStatus{}, fmt.Errorf("applying cronjob: %w", err)
	}

	return ritualsv1.MaintenanceStatus{Schedule: cron, CronJobName: md.GetName()}, nil
}

// applyCronJob server-side-applies the CronJob the Maintenance owns. Only the
// fields adept decides are set here; the job template is the Definition's, read
// into its apply configuration rather than converted from the whole object,
// which would claim every zero value as deliberate.
func (r *MaintenanceManager) applyCronJob(ctx context.Context, md *ritualsv1.Maintenance, def *ritualsv1.Definition, cronSchedule, timeZone string) error {
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

func (r *MaintenanceManager) deleteCronJob(ctx context.Context, md *ritualsv1.Maintenance) error {
	cj := &batchv1.CronJob{}

	err := r.Get(ctx, client.ObjectKey{Name: md.GetName(), Namespace: md.GetNamespace()}, cj)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("fetching job for deletion: %w", err)
	}

	// we only delete things we control
	if !metav1.IsControlledBy(cj, md) {
		return nil
	}

	err = r.Delete(ctx, cj)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting cronjob: %w", err)
	}

	return nil
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

// MaintenanceWindowMapFunc maps a window to the Maintenances using it, by name
// and, when it is the default, by empty spec.window. Windows are cluster-scoped,
// so this lists across namespaces.
func (r *MaintenanceManager) MaintenanceWindowMapFunc(ctx context.Context, o client.Object) []ctrl.Request {
	return maintenanceForWindow(ctx, r.Client, r.Log, o)
}

// DefinitionMapFunc maps a Definition to the Maintenances in its namespace
// naming it, so repairing a ritual heals its instances.
func (r *MaintenanceManager) DefinitionMapFunc(ctx context.Context, o client.Object) []ctrl.Request {
	list := &ritualsv1.MaintenanceList{}
	if err := r.List(ctx, list, client.InNamespace(o.GetNamespace())); err != nil {
		r.Log.Error(err, "listing maintenances for a ritual", "definition", client.ObjectKeyFromObject(o))
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

// SetupWithManager wires the controller: watch Maintenance, the CronJobs it
// owns, and the windows and Definitions they resolve through.
func (r *MaintenanceManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&ritualsv1.Maintenance{}).
		Owns(&batchv1.CronJob{}).
		Watches(&ritualsv1.MaintenanceWindow{}, handler.EnqueueRequestsFromMapFunc(r.MaintenanceWindowMapFunc)).
		Watches(&ritualsv1.Definition{}, handler.EnqueueRequestsFromMapFunc(r.DefinitionMapFunc)).
		Complete(r)
}

// resolveWindow returns the window a maintenance runs in: the one it names, or
// the one marked as the default when it names none.
func resolveWindow(ctx context.Context, c client.Client, md *ritualsv1.Maintenance) (ritualsv1.MaintenanceWindow, error) {
	wl := &ritualsv1.MaintenanceWindowList{}
	if err := c.List(ctx, wl); err != nil {
		return ritualsv1.MaintenanceWindow{}, fmt.Errorf("listing maintenance windows: %w", err)
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
			return ritualsv1.MaintenanceWindow{}, fmt.Errorf("no maintenance window is marked as the default")
		}
		return ritualsv1.MaintenanceWindow{}, fmt.Errorf("no maintenance window %q", md.Spec.Window)
	}

	return window, nil
}

// spreadIdentity is what the schedule package spreads an instance by. The
// CronJob's schedule and anything timed against it must pass the same one, or
// they land in different minutes of the window.
func spreadIdentity(md *ritualsv1.Maintenance) string {
	return fmt.Sprintf("%s/%s", md.GetNamespace(), md.GetName())
}

// maintenanceForWindow maps a window to the Maintenances using it, by name and,
// when it is the default, by empty spec.window. Windows are cluster-scoped, so
// this lists across namespaces.
func maintenanceForWindow(ctx context.Context, c client.Client, log logr.Logger, o client.Object) []ctrl.Request {
	window, ok := o.(*ritualsv1.MaintenanceWindow)
	if !ok {
		return nil
	}

	list := &ritualsv1.MaintenanceList{}
	if err := c.List(ctx, list); err != nil {
		log.Error(err, "listing maintenances for a window", "window", window.GetName())
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
