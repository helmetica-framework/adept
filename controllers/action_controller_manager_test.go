package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
)

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(ritualsv1.AddToScheme(scheme))
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "Database"},
		&unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "DatabaseList"},
		&unstructured.UnstructuredList{})
	return scheme
}

// claim is the unstructured claim object the Action points at; instanceNs
// lands in status.instanceNamespace ("" leaves the status empty).
func claim(ns, instanceNs string) *unstructured.Unstructured {
	c := &unstructured.Unstructured{}
	c.SetAPIVersion("example.org/v1")
	c.SetKind("Database")
	c.SetNamespace(ns)
	c.SetName("my-claim")
	if instanceNs != "" {
		utilruntime.Must(unstructured.SetNestedField(c.Object, instanceNs, "status", "instanceNamespace"))
	}
	return c
}

func definition(ns string) *ritualsv1.Definition {
	return &ritualsv1.Definition{
		ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: ns},
		Spec: ritualsv1.DefinitionSpec{
			Description: "Restart the application",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "restart", Image: "kubectl"},
							},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		},
	}
}

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name, Labels: labels,
	}}
}

func instanceNS(name string) *corev1.Namespace {
	return namespace(name, map[string]string{instanceNamespaceLabel: "true"})
}

// claimNS is a chryso-managed claim namespace: annotated, but without the
// instance label.
func claimNS(name string) *corev1.Namespace {
	ns := namespace(name, nil)
	ns.Annotations = map[string]string{chrysoAnnotationPrefix + "managed": "true"}
	return ns
}

func action(ns string, status ritualsv1.ActionStatus) *ritualsv1.Action {
	return &ritualsv1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "restart-now", Namespace: ns},
		Spec:       ritualsv1.ActionSpec{Type: "restart", Claim: "my-claim"},
		Status:     status,
	}
}

// claimAction is an Action that names its claim's GVK, as required when the
// Action lives in a claim namespace.
func claimAction(ns string) *ritualsv1.Action {
	act := action(ns, ritualsv1.ActionStatus{})
	act.Spec.ApiVersion = "example.org/v1"
	act.Spec.Kind = "Database"
	return act
}

func newManager(objs ...client.Object) (*ActionManager, client.Client, *events.FakeRecorder) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ritualsv1.Action{}).
		WithObjects(objs...).
		Build()
	rec := events.NewFakeRecorder(8)
	return &ActionManager{Client: c, Scheme: scheme, Recorder: rec}, c, rec
}

// reconcile runs one reconcile of "svc/restart-now" with "svc" set up as an
// instance namespace, so the Job stays in place.
func reconcile(t *testing.T, objs ...client.Object) (client.Client, *events.FakeRecorder, ctrl.Result, error) {
	t.Helper()
	am, c, rec := newManager(append(objs, instanceNS("svc"))...)
	res, err := am.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "restart-now", Namespace: "svc"},
	})
	return c, rec, res, err
}

func getAction(t *testing.T, c client.Client) *ritualsv1.Action {
	t.Helper()
	got := &ritualsv1.Action{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "restart-now", Namespace: "svc"}, got))
	return got
}

func TestReconcile_CreatesJobAndSetsPending(t *testing.T) {
	c, _, _, err := reconcile(t, definition("svc"), action("svc", ritualsv1.ActionStatus{}))
	require.NoError(t, err)

	got := getAction(t, c)
	assert.Equal(t, ritualsv1.ActionPhasePending, got.Status.Phase)
	assert.Equal(t, "restart-now-restart", got.Status.JobName)
	assert.Contains(t, got.Finalizers, actionFinalizer)

	job := &batchv1.Job{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "restart-now-restart", Namespace: "svc"}, job))
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "kubectl", job.Spec.Template.Spec.Containers[0].Image)
	require.Len(t, job.OwnerReferences, 1)
	assert.Equal(t, "Action", job.OwnerReferences[0].Kind)
	assert.Equal(t, "restart-now", job.OwnerReferences[0].Name)
}

func TestReconcile_MissingDefinitionFails(t *testing.T) {
	c, rec, _, err := reconcile(t, action("svc", ritualsv1.ActionStatus{}))
	require.NoError(t, err, "missing definition is terminal, not a retryable error")

	got := getAction(t, c)
	assert.Equal(t, ritualsv1.ActionPhaseFailed, got.Status.Phase)
	assert.Empty(t, got.Status.JobName)

	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "restart")
	default:
		t.Fatal("expected a warning event for the missing definition")
	}
}

func TestReconcile_JobStatusMapping(t *testing.T) {
	tests := []struct {
		name      string
		jobStatus batchv1.JobStatus
		want      ritualsv1.ActionPhase
	}{
		{
			name:      "active job means running",
			jobStatus: batchv1.JobStatus{Active: 1},
			want:      ritualsv1.ActionPhaseRunning,
		},
		{
			name:      "succeeded pods mean succeeded",
			jobStatus: batchv1.JobStatus{Succeeded: 1},
			want:      ritualsv1.ActionPhaseSucceeded,
		},
		{
			name: "failed condition means failed",
			jobStatus: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			}},
			want: ritualsv1.ActionPhaseFailed,
		},
		{
			name:      "no signal stays pending",
			jobStatus: batchv1.JobStatus{},
			want:      ritualsv1.ActionPhasePending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "restart-now-restart", Namespace: "svc"},
				Status:     tt.jobStatus,
			}
			c, _, _, err := reconcile(t, definition("svc"),
				action("svc", ritualsv1.ActionStatus{
					Phase:   ritualsv1.ActionPhasePending,
					JobName: "restart-now-restart",
				}), job)
			require.NoError(t, err)
			assert.Equal(t, tt.want, getAction(t, c).Status.Phase)
		})
	}
}

func TestReconcile_TerminalPhaseIsNoOp(t *testing.T) {
	c, _, _, err := reconcile(t, definition("svc"),
		action("svc", ritualsv1.ActionStatus{
			Phase:   ritualsv1.ActionPhaseSucceeded,
			JobName: "restart-now-restart",
		}))
	require.NoError(t, err)

	jobs := &batchv1.JobList{}
	require.NoError(t, c.List(context.Background(), jobs, client.InNamespace("svc")))
	assert.Empty(t, jobs.Items, "terminal action must not create a new job")
	assert.Equal(t, ritualsv1.ActionPhaseSucceeded, getAction(t, c).Status.Phase)
}

func TestReconcile_AdoptsExistingJob(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "restart-now-restart", Namespace: "svc"},
	}
	c, _, _, err := reconcile(t, definition("svc"), action("svc", ritualsv1.ActionStatus{}), job)
	require.NoError(t, err)

	got := getAction(t, c)
	assert.Equal(t, "restart-now-restart", got.Status.JobName)
	assert.Equal(t, ritualsv1.ActionPhasePending, got.Status.Phase)
}

func TestReconcile_ActionGoneIsNoError(t *testing.T) {
	_, _, _, err := reconcile(t)
	require.NoError(t, err)
}

func TestInstanceNamespace(t *testing.T) {
	ctx := context.Background()

	t.Run("instance namespace runs in place", func(t *testing.T) {
		am, _, _ := newManager(instanceNS("svc"))
		got, err := am.instanceNamespace(ctx, action("svc", ritualsv1.ActionStatus{}))
		require.NoError(t, err)
		assert.Equal(t, "svc", got)
	})

	t.Run("unmanaged namespace runs in place even with a claim reference", func(t *testing.T) {
		am, _, _ := newManager(namespace("svc", nil))
		got, err := am.instanceNamespace(ctx, claimAction("svc"))
		require.NoError(t, err)
		assert.Equal(t, "svc", got)
	})

	t.Run("claim namespace reads the claim's status.instanceNamespace", func(t *testing.T) {
		am, _, _ := newManager(claimNS("svc"), claim("svc", "db-instance"))
		got, err := am.instanceNamespace(ctx, claimAction("svc"))
		require.NoError(t, err)
		assert.Equal(t, "db-instance", got)
	})

	t.Run("apiVersion without a group is an error", func(t *testing.T) {
		am, _, _ := newManager(claimNS("svc"))
		act := claimAction("svc")
		act.Spec.ApiVersion = "v1"
		_, err := am.instanceNamespace(ctx, act)
		assert.Error(t, err)
	})

	t.Run("missing claim is an error", func(t *testing.T) {
		am, _, _ := newManager(claimNS("svc"))
		_, err := am.instanceNamespace(ctx, claimAction("svc"))
		assert.ErrorContains(t, err, "no claim")
	})

	t.Run("claim without status.instanceNamespace is an error", func(t *testing.T) {
		am, _, _ := newManager(claimNS("svc"), claim("svc", ""))
		_, err := am.instanceNamespace(ctx, claimAction("svc"))
		assert.ErrorContains(t, err, "status.instanceNamespace")
	})

	t.Run("claim namespace without claim GVK is an error", func(t *testing.T) {
		am, _, _ := newManager(claimNS("svc"))
		_, err := am.instanceNamespace(ctx, action("svc", ritualsv1.ActionStatus{}))
		assert.Error(t, err)
	})

	t.Run("missing namespace is an error", func(t *testing.T) {
		am, _, _ := newManager()
		_, err := am.instanceNamespace(ctx, action("svc", ritualsv1.ActionStatus{}))
		assert.Error(t, err)
	})
}

func TestReconcile_ClaimNamespaceCreatesJobInInstanceNamespace(t *testing.T) {
	ctx := context.Background()
	act := claimAction("svc")
	instNs := "db-instance"

	am, c, _ := newManager(claimNS("svc"), claim("svc", instNs), act, definition(instNs))
	_, err := am.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "restart-now", Namespace: "svc"},
	})
	require.NoError(t, err)

	job := &batchv1.Job{}
	require.NoError(t, c.Get(ctx,
		types.NamespacedName{Name: "restart-now-restart", Namespace: instNs}, job))
	assert.Empty(t, job.OwnerReferences, "cross-namespace owner refs are invalid")
	assert.Equal(t, ritualsv1.ActionPhasePending, getAction(t, c).Status.Phase)

	// The annotations must route Job events back to the Action.
	assert.Equal(t, []ctrl.Request{
		{NamespacedName: types.NamespacedName{Name: "restart-now", Namespace: "svc"}},
	}, JobMapFunc(ctx, job))
}

func TestReconcile_ClaimResolutionErrorBubblesIntoStatus(t *testing.T) {
	ctx := context.Background()

	// Claim namespace, but no claim object: message lands in status and the
	// error is returned so the workqueue retries with backoff.
	am, c, rec := newManager(claimNS("svc"), claimAction("svc"))
	_, err := am.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "restart-now", Namespace: "svc"},
	})
	require.ErrorContains(t, err, "no claim")

	got := getAction(t, c)
	assert.Empty(t, got.Status.Phase, "resolution failure is retried, not terminal")
	assert.Contains(t, got.Status.Message, "no claim")

	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "ClaimResolveFailed")
	default:
		t.Fatal("expected a warning event for the failed claim resolution")
	}

	// A backoff retry with the same failure keeps the message and stays
	// quiet: no second event, no status churn.
	_, err = am.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "restart-now", Namespace: "svc"},
	})
	require.ErrorContains(t, err, "no claim")
	assert.Equal(t, got.Status.Message, getAction(t, c).Status.Message)
	select {
	case ev := <-rec.Events:
		t.Fatalf("unchanged failure must not emit another event, got %q", ev)
	default:
	}
}

func TestJobMapFunc_IgnoresForeignJobs(t *testing.T) {
	assert.Empty(t, JobMapFunc(context.Background(), &batchv1.Job{}))
}

func TestReconcile_DeletePropagatesToJob(t *testing.T) {
	ctx := context.Background()
	act := action("svc", ritualsv1.ActionStatus{
		Phase:   ritualsv1.ActionPhaseRunning,
		JobName: "restart-now-restart",
	})
	act.Finalizers = []string{actionFinalizer}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "restart-now-restart", Namespace: "svc"}}

	am, c, _ := newManager(instanceNS("svc"), act, job)
	require.NoError(t, c.Delete(ctx, act), "delete only sets the deletion timestamp while the finalizer holds")

	_, err := am.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "restart-now", Namespace: "svc"},
	})
	require.NoError(t, err)

	err = c.Get(ctx, types.NamespacedName{Name: "restart-now-restart", Namespace: "svc"}, &batchv1.Job{})
	assert.True(t, apierrors.IsNotFound(err), "job must be deleted with the action")

	err = c.Get(ctx, types.NamespacedName{Name: "restart-now", Namespace: "svc"}, &ritualsv1.Action{})
	assert.True(t, apierrors.IsNotFound(err), "finalizer removed, action gone")
}
