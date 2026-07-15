package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	return scheme
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

func action(ns string, status ritualsv1.ActionStatus) *ritualsv1.Action {
	return &ritualsv1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "restart-now", Namespace: ns},
		Spec:       ritualsv1.ActionSpec{Type: "restart", Claim: "my-claim"},
		Status:     status,
	}
}

func reconcile(t *testing.T, objs ...client.Object) (client.Client, *events.FakeRecorder, ctrl.Result, error) {
	t.Helper()
	scheme := newTestScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ritualsv1.Action{}).
		WithObjects(objs...).
		Build()
	rec := events.NewFakeRecorder(8)
	am := &ActionManager{Client: c, Scheme: scheme, Recorder: rec}
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
