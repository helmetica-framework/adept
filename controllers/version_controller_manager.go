package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

// Writing a claim's status needs no marker here. Claims are dynamic kinds, so
// no static rule can name them; the grant comes from the aggregated ClusterRole
// chrysopoeia keeps in step with the CRDs it generates, bound in
// config/rbac/chrysopoeia_claims_edit_role_binding.yaml.

// The claim an instance namespace was rendered for, as chrysopoeia records it
// there (release_controller.go:197). Note the camelCase in the first one.
const (
	claimAPIVersionAnnotation = "chrysopoeia.io/claim-apiVersion"
	claimKindAnnotation       = "chrysopoeia.io/claim-kind"
	claimNamespaceAnnotation  = "chrysopoeia.io/claim-namespace"
	claimNameAnnotation       = "chrysopoeia.io/claim-name"
)

// bumpNowAnnotation asks for a version bump straight away instead of at the
// next maintenance. Any value works: the controller acts when it differs from
// status.observedBumpRequest, so re-applying the same value does nothing and a
// stale annotation is inert. It is never removed by the controller, which would
// fight the chart that rendered the object.
const bumpNowAnnotation = "rituals.helmetica.io/bump-now"

// ManagedLabel marks the CRDs chrysopoeia generates, with an empty value
// (customresourcedefinitionsource_controller_manager.go:341). Exported because the
// manager's cache is filtered by it too, and the two selectors must agree.
const ManagedLabel = "chrysopoeia.io/managed"

// claimRef identifies the claim behind an instance, enough to Get it as an
// unstructured object.
type claimRef struct {
	GVK       schema.GroupVersionKind
	Namespace string
	Name      string
}

// claimRefFor reads the claim an instance namespace belongs to. A namespace
// with none of the annotations is a plain helm install rather than a managed
// instance, so it reports false and no error: there is no claim to bump and
// nothing is wrong. A partial set is a bug, since chrysopoeia writes them all
// in one apply.
func (r *VersionManager) claimRefFor(ctx context.Context, namespace string) (claimRef, bool, error) {
	ns := &corev1.Namespace{}

	err := r.Get(ctx, client.ObjectKey{Name: namespace}, ns)
	if err != nil {
		return claimRef{}, false, fmt.Errorf("getting instance namespace: %w", err)
	}

	req := []string{
		claimAPIVersionAnnotation,
		claimKindAnnotation,
		claimNameAnnotation,
		claimNamespaceAnnotation,
	}

	missing := []string{}
	for _, ann := range req {
		if ns.GetAnnotations()[ann] == "" {
			missing = append(missing, ann)
		}
	}

	// not a managed ns, it's fine
	if len(missing) == len(req) {
		return claimRef{}, false, nil
	}

	if len(missing) > 0 {
		return claimRef{}, false, fmt.Errorf("namespace %s is missing chrysopoeia annotations: %s", namespace, strings.Join(missing, ", "))
	}

	// FromAPIVersionAndKind swallows the error, so we go the long way
	gv, err := schema.ParseGroupVersion(ns.GetAnnotations()[claimAPIVersionAnnotation])
	if err != nil {
		return claimRef{}, false, fmt.Errorf("parsing the claim apiVersion of namespace %s: %w", namespace, err)
	}

	gvk := gv.WithKind(ns.GetAnnotations()[claimKindAnnotation])

	ref := claimRef{
		Name:      ns.GetAnnotations()[claimNameAnnotation],
		Namespace: ns.GetAnnotations()[claimNamespaceAnnotation],
		GVK:       gvk,
	}

	return ref, true, nil
}

// claimFor gets the claim a ref points at. Claims are dynamic kinds, so this is
// unstructured, the same way ActionManager.instanceNamespace reads one. The
// error keeps IsNotFound: a claim that is not there yet is retryable.
func (r *VersionManager) claimFor(ctx context.Context, ref claimRef) (*unstructured.Unstructured, error) {
	claim := &unstructured.Unstructured{}

	claim.SetGroupVersionKind(ref.GVK)

	err := r.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}, claim)
	if err != nil {
		return nil, fmt.Errorf("getting claim: %w", err)
	}

	return claim, nil
}

// claimCRDFor finds the generated CRD behind a claim kind. It matches rather
// than deriving the name, which is plural.group and only the CRD knows its own
// plural.
func (r *VersionManager) claimCRDFor(ctx context.Context, ref claimRef) (*apiextv1.CustomResourceDefinition, error) {
	crds := &apiextv1.CustomResourceDefinitionList{}
	if err := r.List(ctx, crds, client.MatchingLabels{ManagedLabel: ""}); err != nil {
		return nil, fmt.Errorf("listing claim CRDs: %w", err)
	}

	i := slices.IndexFunc(crds.Items, func(crd apiextv1.CustomResourceDefinition) bool {
		return crd.Spec.Group == ref.GVK.Group && crd.Spec.Names.Kind == ref.GVK.Kind
	})
	if i < 0 {
		return nil, fmt.Errorf("no CustomResourceDefinition for %s in %s", ref.GVK.Kind, ref.GVK.Group)
	}

	return &crds.Items[i], nil
}

// newestVersion is the newest version the claim's schema allows. Chrysopoeia
// sorts the enum descending when it generates the CRD
// (customresourcedefinitionsource_controller_manager.go:309), so adept takes
// the first entry and never decides what "newest" means.
func newestVersion(crd *apiextv1.CustomResourceDefinition, ref claimRef) (string, error) {
	i := slices.IndexFunc(crd.Spec.Versions, func(v apiextv1.CustomResourceDefinitionVersion) bool {
		return v.Name == ref.GVK.Version
	})
	if i < 0 {
		return "", fmt.Errorf("%s has no version %s", crd.GetName(), ref.GVK.Version)
	}

	props := crd.Spec.Versions[i].Schema
	if props == nil || props.OpenAPIV3Schema == nil {
		return "", fmt.Errorf("%s %s has no schema", ref.GVK.Kind, ref.GVK.Version)
	}

	spec, ok := props.OpenAPIV3Schema.Properties["spec"]
	if !ok {
		return "", fmt.Errorf("%s has no spec in its schema", ref.GVK.Kind)
	}

	version, ok := spec.Properties["version"]
	if !ok || len(version.Enum) == 0 {
		return "", fmt.Errorf("%s has no versions to select from", ref.GVK.Kind)
	}

	var newest string
	if err := json.Unmarshal(version.Enum[0].Raw, &newest); err != nil {
		return "", fmt.Errorf("reading the newest version of %s: %w", ref.GVK.Kind, err)
	}

	return newest, nil
}

// pinnedVersion is the version the user pinned on the claim, or "" when they
// left the choice to the framework. Absent and empty mean the same thing: an
// empty spec.version marks a managed instance.
func pinnedVersion(claim *unstructured.Unstructured) (string, error) {
	version, found, err := unstructured.NestedString(claim.Object, "spec", "version")
	if err != nil {
		return "", fmt.Errorf("can't get claim version: %w", err)
	}

	if !found {
		return "", nil
	}

	return version, nil
}

// needsBump reports whether the claim should be moved onto newest. Rewriting an
// unchanged version would churn the claim on every reconcile, and each write is
// a revision downstream.
//
// A stale status.version on a pinned claim is left alone rather than corrected:
// resolution reads spec.version first, so nothing downstream reads the stale
// one.
func needsBump(claim *unstructured.Unstructured, newest string) (bool, error) {
	pinned, err := pinnedVersion(claim)
	if err != nil {
		return false, err
	}

	if pinned != "" {
		return false, nil
	}

	version, _, err := unstructured.NestedString(claim.Object, "status", "version")
	if err != nil {
		return false, fmt.Errorf("can't get claim status version: %w", err)
	}

	return newest != version, nil
}

// bumpClaim writes newest to the claim's status.version. It applies a fresh
// object carrying only that field; applying the claim as read would take
// ownership of everything on it, including what chrysopoeia owns.
func (r *VersionManager) bumpClaim(ctx context.Context, claim *unstructured.Unstructured, newest string) error {
	sclaim := &unstructured.Unstructured{}
	sclaim.SetGroupVersionKind(claim.GetObjectKind().GroupVersionKind())
	sclaim.SetName(claim.GetName())
	sclaim.SetNamespace(claim.GetNamespace())

	err := unstructured.SetNestedField(sclaim.Object, newest, "status", "version")
	if err != nil {
		return fmt.Errorf("setting status.version on claim %s: %w", claim.GetName(), err)
	}

	return r.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(sclaim), client.ForceOwnership, versionFieldOwner)
}

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

	// manual bump
	if request, outstanding := bumpRequested(md); outstanding {
		if _, err := r.bumpVersion(ctx, md, log); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.recordBumpRequest(ctx, md, request); err != nil {
			return ctrl.Result{}, err
		}
	}

	now := r.now()
	identity := spreadIdentity(md)

	occurrence, open, err := schedule.BumpDue(window.Spec, identity, now)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving the maintenance to act on: %w", err)
	}

	if open && !bumpedFor(md, occurrence) {
		if err := r.bump(ctx, md, occurrence, log); err != nil {
			return ctrl.Result{}, err
		}
	}

	bump, err := schedule.NextBump(window.Spec, identity, now)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving the next bump: %w", err)
	}

	return ctrl.Result{RequeueAfter: time.Until(bump)}, nil
}

// bump is the scheduled caller: it moves the version and watermarks the
// maintenance it acted for. Nothing written means no watermark, because the
// watermark says a version was written.
func (r *VersionManager) bump(ctx context.Context, md *ritualsv1.Maintenance, occurrence time.Time, log logr.Logger) error {
	bumped, err := r.bumpVersion(ctx, md, log.WithValues("maintenance", occurrence))
	if err != nil || !bumped {
		return err
	}

	return r.recordBump(ctx, md, occurrence)
}

// bumpRequested reports the bump-now annotation's value and whether acting on
// it is still outstanding. A request already in status.observedBumpRequest has
// been served: the annotation stays on the object, and re-applying the same
// value must not move the version again.
func bumpRequested(md *ritualsv1.Maintenance) (string, bool) {
	req, ok := md.GetAnnotations()[bumpNowAnnotation]
	if !ok {
		return "", false
	}

	return req, req != md.Status.ObservedBumpRequest
}

// recordBumpRequest writes the manual watermark. It says the request was
// served, not that a version was written, which is where it differs from
// recordBump: a claim that is already newest still has to be recorded, or the
// same annotation is re-checked on every reconcile for as long as it is there.
func (r *VersionManager) recordBumpRequest(ctx context.Context, md *ritualsv1.Maintenance, request string) error {
	status := ritualsacv1.Maintenance(md.Name, md.Namespace).
		WithStatus(ritualsacv1.MaintenanceStatus().
			WithObservedBumpRequest(request))

	if err := r.Status().Apply(ctx, status, versionFieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("recording the version update: %w", err)
	}
	return nil
}

// bumpVersion moves the instance's claim onto the newest version its schema
// allows and reports whether it wrote one. Every reason to do nothing reports
// false, because each caller watermarks on its own terms.
func (r *VersionManager) bumpVersion(ctx context.Context, md *ritualsv1.Maintenance, log logr.Logger) (bool, error) {
	ref, managed, err := r.claimRefFor(ctx, md.GetNamespace())
	if err != nil {
		return false, err
	}

	if !managed {
		log.V(1).Info("not a chrysopoeia instance, no claim to bump")
		return false, nil
	}

	claim, err := r.claimFor(ctx, ref)
	if err != nil {
		return false, err
	}

	crd, err := r.claimCRDFor(ctx, ref)
	if err != nil {
		return false, err
	}

	newest, err := newestVersion(crd, ref)
	if err != nil {
		return false, err
	}

	needed, err := needsBump(claim, newest)
	if err != nil {
		return false, err
	}

	if !needed {
		log.V(1).Info("claim needs no bump", "claim", ref.Name, "newest", newest)
		return false, nil
	}

	if err := r.bumpClaim(ctx, claim, newest); err != nil {
		return false, err
	}

	log.Info("moved the instance's version", "claim", ref.Name, "version", newest)

	return true, nil
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
