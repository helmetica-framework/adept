package controllers

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
)

// maxNameLen is the Kubernetes name limit; Job names are clamped to it.
const maxNameLen = 63

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
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile drives an Action to a terminal phase: it creates one Job from the
// Definition named by spec.type, then maps that Job's status onto the Action.
// Succeeded and Failed are terminal — the Action is never re-run.
func (r *ActionManager) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	act := &ritualsv1.Action{}
	err := r.Get(ctx, req.NamespacedName, act)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if act.Status.Phase == ritualsv1.ActionPhaseSucceeded ||
		act.Status.Phase == ritualsv1.ActionPhaseFailed {
		return ctrl.Result{}, nil
	}

	if act.Status.JobName == "" {
		return ctrl.Result{}, r.createJob(ctx, act)
	}
	return ctrl.Result{}, r.trackJob(ctx, act)
}

// createJob instantiates the Definition's job template as an owned Job and moves
// the Action to Pending. A missing Definition is terminal (warning event +
// Failed); an already-existing Job is adopted.
func (r *ActionManager) createJob(ctx context.Context, act *ritualsv1.Action) error {
	actD := &ritualsv1.Definition{}
	// TODO: with actual claims we'll have to round trip from the claim
	// ns to the instance ns.
	err := r.Get(ctx, client.ObjectKey{Name: act.Spec.Type, Namespace: act.Namespace}, actD)
	if apierrors.IsNotFound(err) {
		// Missing Definition is terminal: warn and fail, don't requeue.
		r.Recorder.Eventf(act, nil, corev1.EventTypeWarning, "DefinitionNotFound", "Create",
			"no Definition %q in namespace %s", act.Spec.Type, act.Namespace)
		act.Status.Phase = ritualsv1.ActionPhaseFailed
		return r.updateStatus(ctx, act)
	}
	if err != nil {
		return fmt.Errorf("getting the action definition: %w", err)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(act),
			Namespace: act.Namespace,
			Labels:    actD.Spec.JobTemplate.Labels,
		},
		Spec: *actD.Spec.JobTemplate.Spec.DeepCopy(),
	}
	if err := ctrl.SetControllerReference(act, job, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	err = r.Create(ctx, job)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating job: %w", err)
	}

	act.Status.JobName = job.GetName()
	act.Status.Phase = ritualsv1.ActionPhasePending
	return r.updateStatus(ctx, act)
}

// trackJob maps the owned Job's status onto the Action phase. A deleted Job
// leaves the phase unchanged; the status is written only when the phase moves.
func (r *ActionManager) trackJob(ctx context.Context, act *ritualsv1.Action) error {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      act.Status.JobName,
			Namespace: act.GetNamespace(),
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

// jobName returns "<action-name>-<spec.type>" truncated to the Kubernetes name
// limit. Names are DNS labels (ASCII), so a byte cut is safe here.
func jobName(action *ritualsv1.Action) string {
	name := fmt.Sprintf("%s-%s", action.Name, action.Spec.Type)
	if len(name) > maxNameLen {
		name = name[:maxNameLen]
	}
	return name
}

// SetupWithManager wires the controller: watch Action, own Job.
func (r *ActionManager) SetupWithManager(name string, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&ritualsv1.Action{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
