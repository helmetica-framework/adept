package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
	ritualsacv1 "github.com/helmetica-framework/adept/applyconfiguration/api/v1"
	"github.com/helmetica-framework/adept/schedule"
)

// versionFieldOwner is this controller's server-side-apply field manager. It is
// its own so that what it writes and what MaintenanceManager writes can share
// an object without either clobbering the other.
const versionFieldOwner = client.FieldOwner("adept:maintenance-version")

// TODO(human): RBAC to patch claim status. Claims are dynamic kinds, so this
// cannot be a static marker for a known group (Q1).

// VersionManager moves an instance onto the newest version its claim allows, a
// lead ahead of that instance's maintenance so the ritual runs against the
// version it is meant to.
//
// It keys off Maintenance rather than the claim: claims are dynamic kinds and
// watching them needs a dynamic informer, while a Maintenance already ties an
// instance namespace to a window, and the namespace carries the claim it
// belongs to in its chrysopoeia annotations.
type VersionManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Log      logr.Logger

	// Now is the clock. Nil means time.Now; tests set it so that whether a
	// window is open does not depend on the day they run.
	Now func() time.Time
}

func (r *VersionManager) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// Reconcile wakes at the bump ahead of an instance's maintenance and requeues
// for the next one. Nothing else here holds a timer: the CronJob that runs the
// ritual is fired by Kubernetes, but a version bump has nothing firing it.
func (r *VersionManager) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("maintenance", req.NamespacedName)

	md := &ritualsv1.Maintenance{}
	err := r.Get(ctx, req.NamespacedName, md)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("maintenance is gone, nothing to do")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !md.GetDeletionTimestamp().IsZero() {
		log.V(1).Info("maintenance is being deleted, nothing to do")
		return ctrl.Result{}, nil
	}

	window, err := resolveWindow(ctx, r.Client, md)
	if err != nil {
		return ctrl.Result{}, err
	}

	now := r.now()
	identity := spreadIdentity(md)

	occurrence, open, err := schedule.BumpDue(window.Spec, identity, now)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving the maintenance to act on: %w", err)
	}

	if open && !bumpedFor(md, occurrence) {
		// TODO(human): resolve the claim from the instance namespace's
		// chrysopoeia annotations and write the newest version to its status.
		// Blocked on chrysopoeia U6, U7 and U8, and on the RBAC above.

		if err := r.recordBump(ctx, md, occurrence); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("moved the instance's version", "maintenance", occurrence)
	}

	bump, err := schedule.NextBump(window.Spec, identity, now)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving the next bump: %w", err)
	}

	return ctrl.Result{RequeueAfter: time.Until(bump)}, nil
}

// bumpedFor reports whether this maintenance has already been acted on. The
// wake-up is never the evidence: a restart re-lists every object, so a
// controller that acted on being scheduled would move the whole fleet at once.
func bumpedFor(md *ritualsv1.Maintenance, occurrence time.Time) bool {
	return md.Status.VersionUpdatedFor != nil && md.Status.VersionUpdatedFor.Time.Equal(occurrence)
}

// recordBump writes the watermark. Its own field manager, so it can share the
// status with MaintenanceManager without either taking the other's fields.
func (r *VersionManager) recordBump(ctx context.Context, md *ritualsv1.Maintenance, occurrence time.Time) error {
	status := ritualsacv1.Maintenance(md.Name, md.Namespace).
		WithStatus(ritualsacv1.MaintenanceStatus().
			WithVersionUpdatedFor(metav1.NewTime(occurrence)))

	if err := r.Status().Apply(ctx, status, versionFieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("recording the version update: %w", err)
	}
	return nil
}

// MaintenanceWindowMapFunc maps a window to the Maintenances using it. A moved
// window moves the bump with it; without this the pending wake-up would fire
// against the old schedule.
func (r *VersionManager) MaintenanceWindowMapFunc(ctx context.Context, o client.Object) []ctrl.Request {
	return maintenanceForWindow(ctx, r.Client, r.Log, o)
}

// SetupWithManager wires the controller: watch Maintenance and the windows they
// resolve through. No CronJobs: this controller writes none.
func (r *VersionManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&ritualsv1.Maintenance{}).
		Watches(&ritualsv1.MaintenanceWindow{}, handler.EnqueueRequestsFromMapFunc(r.MaintenanceWindowMapFunc)).
		Complete(r)
}
