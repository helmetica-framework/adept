package controllers

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
)

const (
	// maxNameLen is the Kubernetes DNS label limit; Job names are clamped
	// to it.
	maxNameLen = 63

	instanceNamespaceLabel = "chrysopoeia.io/instance"

	serviceAccount = "instance-admin"

	// actionNameAnnotation and actionNamespaceAnnotation point a Job back to
	// the Action that created it, so Job events can be mapped to reconcile
	// requests across namespaces (owner refs don't work cross-namespace).
	actionNameAnnotation      = "rituals.helmetica.io/action-name"
	actionNamespaceAnnotation = "rituals.helmetica.io/action-namespace"

	// actionFinalizer guards Job cleanup: owner references don't work across
	// namespaces, so the Job is deleted explicitly when the Action goes away.
	actionFinalizer = "rituals.helmetica.io/job-cleanup"
)

// ActionManager reconciles Action objects: it creates a Job from the referenced
// Definition and tracks the Job to a terminal Action phase.
type ActionManager struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=rituals.helmetica.io,resources=definitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=rituals.helmetica.io,resources=actions,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=rituals.helmetica.io,resources=actions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile drives an Action to a terminal phase: it creates one Job from the
// Definition named by spec.type, then maps that Job's status onto the Action.
// Succeeded and Failed are terminal — the Action is never re-run. Instance
// namespace resolution failures are not terminal: they surface in
// status.message and the error is returned so the workqueue retries with
// exponential backoff.
func (r *ActionManager) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	act := &ritualsv1.Action{}
	err := r.Get(ctx, req.NamespacedName, act)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Deletion before the terminal check: even a finished Action must clean
	// up its Job on the way out.
	if !act.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, r.finalize(ctx, act)
	}

	if act.Status.Phase == ritualsv1.ActionPhaseSucceeded ||
		act.Status.Phase == ritualsv1.ActionPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Resolution failures (bad spec, missing claim, claim still provisioning)
	// surface in status.message and are retried with exponential backoff by
	// returning the error. Event and status write only fire when the message
	// changes, so backoff retries don't spam.
	jobNs, err := r.instanceNamespace(ctx, act)
	if err != nil {
		if act.Status.Message != err.Error() {
			r.Recorder.Eventf(act, nil, corev1.EventTypeWarning, "ClaimResolveFailed", "Resolve", "%s", err.Error())
			act.Status.Message = err.Error()
			if uerr := r.updateStatus(ctx, act); uerr != nil {
				return ctrl.Result{}, uerr
			}
		}
		return ctrl.Result{}, err
	}
	// Stale resolution message clears on the next status write.
	act.Status.Message = ""

	if act.Status.JobName == "" {
		if controllerutil.AddFinalizer(act, actionFinalizer) {
			if err := r.Update(ctx, act); err != nil {
				return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
			}
		}
		return ctrl.Result{}, r.createJob(ctx, act, jobNs)
	}
	return ctrl.Result{}, r.trackJob(ctx, act, jobNs)
}

// createJob instantiates the Definition's job template as a Job in the given
// instance namespace and moves the Action to Pending. A missing Definition is
// terminal (warning event + Failed); an already-existing Job is adopted. The
// Job is owned by the Action only when both live in the same namespace —
// cross-namespace owner references are invalid in Kubernetes.
func (r *ActionManager) createJob(ctx context.Context, act *ritualsv1.Action, ns string) error {
	actD := &ritualsv1.Definition{}
	err := r.Get(ctx, client.ObjectKey{Name: act.Spec.Type, Namespace: ns}, actD)
	if apierrors.IsNotFound(err) {
		// Missing Definition is terminal: warn and fail, don't requeue.
		r.Recorder.Eventf(act, nil, corev1.EventTypeWarning, "DefinitionNotFound", "Create",
			"no Definition %q in namespace %s", act.Spec.Type, ns)
		act.Status.Phase = ritualsv1.ActionPhaseFailed
		act.Status.Message = fmt.Sprintf("no Definition %q in namespace %s", act.Spec.Type, ns)
		return r.updateStatus(ctx, act)
	}
	if err != nil {
		return fmt.Errorf("getting the action definition: %w", err)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(act),
			Namespace: ns,
			Labels:    actD.Spec.JobTemplate.Labels,
			Annotations: map[string]string{
				actionNameAnnotation:      act.GetName(),
				actionNamespaceAnnotation: act.GetNamespace(),
			},
		},
		Spec: *actD.Spec.JobTemplate.Spec.DeepCopy(),
	}

	if ns == act.GetNamespace() {
		if err := ctrl.SetControllerReference(act, job, r.Scheme); err != nil {
			return fmt.Errorf("setting owner reference: %w", err)
		}
	}

	job.Spec.Template.Spec.ServiceAccountName = serviceAccount

	err = r.Create(ctx, job)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating job: %w", err)
	}

	act.Status.JobName = job.GetName()
	act.Status.Phase = ritualsv1.ActionPhasePending
	return r.updateStatus(ctx, act)
}

// finalize deletes the Action's Job (background propagation so pods go too)
// and removes the finalizer. An already-gone Job is not an error, and neither
// is a claim or namespace that is already deleted — the Job's namespace is
// torn down with them, and the finalizer must not wedge the Action forever.
func (r *ActionManager) finalize(ctx context.Context, act *ritualsv1.Action) error {
	if act.Status.JobName != "" {
		ns, err := r.instanceNamespace(ctx, act)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil {
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name:      act.Status.JobName,
				Namespace: ns,
			}}
			err = r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting job: %w", err)
			}
		}
	}
	if controllerutil.RemoveFinalizer(act, actionFinalizer) {
		return r.Update(ctx, act)
	}
	return nil
}

// trackJob maps the Action's Job status onto the Action phase. A deleted Job
// leaves the phase unchanged; the status is written only when the phase moves.
func (r *ActionManager) trackJob(ctx context.Context, act *ritualsv1.Action, ns string) error {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      act.Status.JobName,
			Namespace: ns,
		},
	}

	err := r.Get(ctx, client.ObjectKeyFromObject(job), job)
	if apierrors.IsNotFound(err) {
		// Job gone (owner deleted): leave phase as-is.
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting job: %w", err)
	}

	// Order matters: a failed job stays failed even if some pods succeeded.
	phase := act.Status.Phase
	switch {
	case jobFailed(job):
		phase = ritualsv1.ActionPhaseFailed
	case job.Status.Succeeded > 0:
		phase = ritualsv1.ActionPhaseSucceeded
	case job.Status.Active > 0:
		phase = ritualsv1.ActionPhaseRunning
	}
	if phase == act.Status.Phase {
		return nil
	}
	act.Status.Phase = phase
	return r.updateStatus(ctx, act)
}

func jobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *ActionManager) updateStatus(ctx context.Context, act *ritualsv1.Action) error {
	if err := r.Status().Update(ctx, act); err != nil {
		return fmt.Errorf("updating action status: %w", err)
	}
	return nil
}

// jobName returns "<action-name>-<spec.type>" clamped to the Kubernetes name
// limit.
func jobName(action *ritualsv1.Action) string {
	return clampName(fmt.Sprintf("%s-%s", action.Name, action.Spec.Type))
}

// clampName truncates name to the DNS label limit. Names are DNS labels
// (ASCII), so a byte cut is safe; a trailing "-" left by the cut would be an
// invalid label, so it is trimmed.
func clampName(name string) string {
	if len(name) > maxNameLen {
		name = strings.TrimRight(name[:maxNameLen], "-")
	}
	return name
}

// SetupWithManager wires the controller: watch Action, and map Job events back
// to their Action via the action annotations.
func (r *ActionManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&ritualsv1.Action{}).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(JobMapFunc)).
		Complete(r)
}

// JobMapFunc maps a Job to the Action named by its action annotations. Jobs
// without them (not created by this controller) are ignored.
func JobMapFunc(ctx context.Context, o client.Object) []ctrl.Request {
	ann := o.GetAnnotations()
	name, ns := ann[actionNameAnnotation], ann[actionNamespaceAnnotation]
	if name == "" || ns == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}}
}

// instanceNamespace resolves the namespace the Action's Definition and Job
// live in. An Action created in an instance namespace (carrying
// instanceNamespaceLabel) runs in place. An Action created in a claim
// namespace runs in the namespace named by the claim's
// status.instanceNamespace; the claim is looked up by spec.claim and its GVK
// (spec.apiVersion + spec.kind).
func (r *ActionManager) instanceNamespace(ctx context.Context, act *ritualsv1.Action) (string, error) {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: act.GetNamespace()}, ns); err != nil {
		return "", fmt.Errorf("getting action namespace: %w", err)
	}

	if _, ok := ns.GetLabels()[instanceNamespaceLabel]; ok {
		return act.GetNamespace(), nil
	}

	gv, err := schema.ParseGroupVersion(act.Spec.ApiVersion)
	if err != nil {
		return "", fmt.Errorf("parsing claim apiVersion: %w", err)
	}
	if act.Spec.Kind == "" || gv.Group == "" || gv.Version == "" {
		return "", fmt.Errorf("spec.kind and spec.apiVersion (group/version) must name the claim in namespace %s", act.GetNamespace())
	}

	gvk := gv.WithKind(act.Spec.Kind)

	claim := unstructured.Unstructured{}
	claim.SetGroupVersionKind(gvk)

	err = r.Get(ctx, client.ObjectKey{Namespace: ns.GetName(), Name: act.Spec.Claim}, &claim)
	if apierrors.IsNotFound(err) {
		// %w keeps IsNotFound visible to callers (finalize relies on it).
		return "", fmt.Errorf("no claim %s %q in namespace %s: %w", gvk.Kind, act.Spec.Claim, ns.GetName(), err)
	}
	if err != nil {
		return "", fmt.Errorf("getting claim: %w", err)
	}

	instanceNs, ok, err := unstructured.NestedString(claim.Object, "status", "instanceNamespace")
	if err != nil {
		return "", fmt.Errorf("reading claim status.instanceNamespace: %w", err)
	}
	if !ok || instanceNs == "" {
		return "", fmt.Errorf("claim %s/%s has no status.instanceNamespace", ns.GetName(), act.Spec.Claim)
	}

	return instanceNs, nil
}
