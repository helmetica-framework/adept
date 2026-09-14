package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
	"github.com/helmetica-framework/adept/schedule"
)

func namedWindow(name string, isDefault bool) *ritualsv1.MaintenanceWindow {
	w := maintenanceWindow()
	w.Name = name
	w.Spec.Default = isDefault
	return w
}

// maintenance lives in "svc"; "svc/maintenance" is its spread
// identity.
func maintenance(windowName string) *ritualsv1.Maintenance {
	return &ritualsv1.Maintenance{
		ObjectMeta: metav1.ObjectMeta{Name: "maintenance", Namespace: "svc"},
		Spec: ritualsv1.MaintenanceSpec{
			Window: windowName,
			Ritual: "maintenance",
		},
	}
}

func maintenanceRitual(ns string) *ritualsv1.Definition {
	d := definition(ns)
	d.Name = "maintenance"
	return d
}

func maintenanceManager(objs ...client.Object) (*MaintenanceManager, client.Client, *events.FakeRecorder) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ritualsv1.Maintenance{}).
		WithObjects(objs...).
		Build()
	rec := events.NewFakeRecorder(8)
	return &MaintenanceManager{Client: c, Scheme: scheme, Recorder: rec, Log: logr.Discard()}, c, rec
}

// reconcileMaintenance runs one reconcile of "svc/maintenance".
func reconcileMaintenance(t *testing.T, objs ...client.Object) (client.Client, *events.FakeRecorder, error) {
	t.Helper()
	m, c, rec := maintenanceManager(objs...)
	_, err := m.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maintenance", Namespace: "svc"},
	})
	return c, rec, err
}

func getCronJob(t *testing.T, c client.Client) *batchv1.CronJob {
	t.Helper()
	var list batchv1.CronJobList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace("svc")))
	require.Len(t, list.Items, 1, "expected exactly one CronJob")
	return &list.Items[0]
}

func getMaintenance(t *testing.T, c client.Client) *ritualsv1.Maintenance {
	t.Helper()
	got := &ritualsv1.Maintenance{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "maintenance", Namespace: "svc"}, got))
	return got
}

func TestMaintenance_CreatesCronJobFromTheNamedWindow(t *testing.T) {
	c, _, err := reconcileMaintenance(t,
		namedWindow("sunday-night", false), maintenanceRitual("svc"), maintenance("sunday-night"))
	require.NoError(t, err)

	cj := getCronJob(t, c)
	require.NotNil(t, cj.Spec.TimeZone)
	assert.Equal(t, "Europe/Zurich", *cj.Spec.TimeZone)
	assert.NotEmpty(t, cj.Spec.Schedule)
	assert.NotEmpty(t, cj.Spec.JobTemplate.Spec.Template.Spec.Containers,
		"the job template must come from the ritual Definition")

	// Owner reference, not a finalizer: both objects share a namespace.
	require.Len(t, cj.OwnerReferences, 1)
	assert.Equal(t, "Maintenance", cj.OwnerReferences[0].Kind)

	md := getMaintenance(t, c)
	assert.Equal(t, cj.Spec.Schedule, md.Status.Schedule,
		"the resolved schedule must be visible on the object an operator reads")
	assert.Equal(t, cj.Name, md.Status.CronJobName)
	assert.Equal(t, md.Generation, md.Status.ObservedGeneration)
	assert.Empty(t, md.Status.Message)
}

func TestMaintenance_EmptyWindowUsesTheDefault(t *testing.T) {
	c, _, err := reconcileMaintenance(t,
		namedWindow("weekend", false), namedWindow("sunday-night", true),
		maintenanceRitual("svc"), maintenance(""))
	require.NoError(t, err)

	assert.NotEmpty(t, getCronJob(t, c).Spec.Schedule)
}

func TestMaintenance_ScheduleMatchesTheSpreadIdentity(t *testing.T) {
	// The identity is namespace/name, not the namespace alone. With the
	// namespace alone, two definitions in one instance namespace resolve to
	// the same minute and fire together.
	c, _, err := reconcileMaintenance(t,
		namedWindow("sunday-night", false), maintenanceRitual("svc"), maintenance("sunday-night"))
	require.NoError(t, err)

	want, tz, err := schedule.CronSchedule(namedWindow("sunday-night", false).Spec, "svc/maintenance")
	require.NoError(t, err)

	cj := getCronJob(t, c)
	assert.Equal(t, want, cj.Spec.Schedule)
	assert.Equal(t, tz, *cj.Spec.TimeZone)
}

func TestMaintenance_SuspendKeepsTheCronJob(t *testing.T) {
	// A vanished CronJob is indistinguishable from a broken controller.
	md := maintenance("sunday-night")
	md.Spec.Suspend = true

	c, _, err := reconcileMaintenance(t, namedWindow("sunday-night", false), maintenanceRitual("svc"), md)
	require.NoError(t, err)

	cj := getCronJob(t, c)
	require.NotNil(t, cj.Spec.Suspend)
	assert.True(t, *cj.Spec.Suspend)
}

func TestMaintenance_UnresolvableInputsAreRetryable(t *testing.T) {
	// A chart may render the definition before the operator creates the window,
	// and a typo must recover without touching the instance. Neither of these is
	// terminal.
	//
	// Both rows are about the window, which is also what guards the resolution
	// order: the ritual is optional, so the window has to be looked at first for
	// these to fail at all. A missing ritual used to be a third row and is now
	// its own set of tests below.
	tests := []struct {
		name string
		objs []client.Object
		want string
	}{
		{
			name: "named window does not exist",
			objs: []client.Object{maintenanceRitual("svc"), maintenance("sunday-night")},
			want: "sunday-night",
		},
		{
			name: "no window is marked default",
			objs: []client.Object{namedWindow("weekend", false), maintenanceRitual("svc"), maintenance("")},
			want: "default",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, rec, err := reconcileMaintenance(t, tt.objs...)
			assert.Error(t, err, "must be retried, not swallowed")

			md := getMaintenance(t, c)
			assert.Contains(t, md.Status.Message, tt.want)
			assert.Empty(t, md.Status.Schedule, "no schedule may be claimed when none resolved")

			select {
			case ev := <-rec.Events:
				assert.Contains(t, ev, "Warning")
			default:
				t.Fatal("expected a warning event")
			}
		})
	}
}

func TestMaintenance_RecoversWhenTheWindowAppears(t *testing.T) {
	m, c, _ := maintenanceManager(maintenanceRitual("svc"), maintenance("sunday-night"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "maintenance", Namespace: "svc"}}

	_, err := m.Reconcile(context.Background(), req)
	require.Error(t, err)

	require.NoError(t, c.Create(context.Background(), namedWindow("sunday-night", false)))

	_, err = m.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Empty(t, getMaintenance(t, c).Status.Message,
		"a stale message must clear once the window resolves")
}

func TestMaintenance_RewritesTheScheduleWhenTheWindowChanges(t *testing.T) {
	// Editing a window must move existing instances rather than orphan a
	// CronJob on the old schedule.
	m, c, _ := maintenanceManager(
		namedWindow("sunday-night", false), maintenanceRitual("svc"), maintenance("sunday-night"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "maintenance", Namespace: "svc"}}

	_, err := m.Reconcile(context.Background(), req)
	require.NoError(t, err)
	before := getCronJob(t, c).Spec.Schedule

	moved := &ritualsv1.MaintenanceWindow{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "sunday-night"}, moved))
	moved.Spec.Time = "02:00"
	require.NoError(t, c.Update(context.Background(), moved))

	_, err = m.Reconcile(context.Background(), req)
	require.NoError(t, err)

	assert.NotEqual(t, before, getCronJob(t, c).Spec.Schedule)
}

func TestMaintenance_SettledDefinitionIsNotRewritten(t *testing.T) {
	m, c, _ := maintenanceManager(
		namedWindow("sunday-night", false), maintenanceRitual("svc"), maintenance("sunday-night"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "maintenance", Namespace: "svc"}}

	_, err := m.Reconcile(context.Background(), req)
	require.NoError(t, err)
	settled := getMaintenance(t, c).ResourceVersion

	_, err = m.Reconcile(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, settled, getMaintenance(t, c).ResourceVersion,
		"an unchanged status must not be written again")
}

func TestMaintenance_MissingRitualStillPublishesTheSchedule(t *testing.T) {
	// The version bump runs off the window whether or not a ritual exists, so a
	// missing one must not cost the operator sight of when their instance moves.
	c, _, err := reconcileMaintenance(t, namedWindow("sunday-night", false), maintenance("sunday-night"))
	require.NoError(t, err, "a missing ritual is a supported setup, not a failure")

	want, _, err := schedule.CronSchedule(namedWindow("sunday-night", false).Spec, "svc/maintenance")
	require.NoError(t, err)

	md := getMaintenance(t, c)
	assert.Equal(t, want, md.Status.Schedule)
	assert.Empty(t, md.Status.CronJobName)
	assert.Contains(t, md.Status.Message, "maintenance",
		"the ritual that is not there must be named, or a typo is invisible")
}

func TestMaintenance_MissingRitualCreatesNoCronJob(t *testing.T) {
	c, _, err := reconcileMaintenance(t, namedWindow("sunday-night", false), maintenance("sunday-night"))
	require.NoError(t, err)

	var list batchv1.CronJobList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace("svc")))
	assert.Empty(t, list.Items)
}

func TestMaintenance_MissingRitualEmitsNoEvent(t *testing.T) {
	// A reagent with no maintenance ritual is a supported setup. Warning on it
	// every time the status moves makes the event stream useless.
	_, rec, err := reconcileMaintenance(t, namedWindow("sunday-night", false), maintenance("sunday-night"))
	require.NoError(t, err)

	select {
	case ev := <-rec.Events:
		t.Fatalf("expected no event, got %q", ev)
	default:
	}
}

func TestMaintenance_MissingRitualDoesNotRequeue(t *testing.T) {
	// Nothing to back off for: DefinitionMapFunc wakes this object when the
	// ritual arrives.
	m, _, _ := maintenanceManager(namedWindow("sunday-night", false), maintenance("sunday-night"))

	res, err := m.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maintenance", Namespace: "svc"}})
	require.NoError(t, err)

	assert.Equal(t, ctrl.Result{}, res)
}

func TestMaintenance_RitualArrivingCreatesTheCronJob(t *testing.T) {
	m, c, _ := maintenanceManager(namedWindow("sunday-night", false), maintenance("sunday-night"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "maintenance", Namespace: "svc"}}

	_, err := m.Reconcile(context.Background(), req)
	require.NoError(t, err)

	require.NoError(t, c.Create(context.Background(), maintenanceRitual("svc")))

	_, err = m.Reconcile(context.Background(), req)
	require.NoError(t, err)

	cj := getCronJob(t, c)
	md := getMaintenance(t, c)
	assert.Equal(t, cj.Name, md.Status.CronJobName)
	assert.Empty(t, md.Status.Message, "the stale message must clear once the ritual arrives")
}

func TestMaintenance_RitualGoingAwayDeletesTheCronJob(t *testing.T) {
	// A chart upgrade that drops the ritual must not leave a CronJob firing a
	// template no Definition declares.
	m, c, _ := maintenanceManager(
		namedWindow("sunday-night", false), maintenanceRitual("svc"), maintenance("sunday-night"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "maintenance", Namespace: "svc"}}

	_, err := m.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, getCronJob(t, c).Spec.Schedule)

	require.NoError(t, c.Delete(context.Background(), maintenanceRitual("svc")))

	_, err = m.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var list batchv1.CronJobList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace("svc")))
	assert.Empty(t, list.Items)
	assert.Empty(t, getMaintenance(t, c).Status.CronJobName)
}

func TestMaintenance_ACronJobItDoesNotOwnIsLeftAlone(t *testing.T) {
	// The CronJob carries the Maintenance's own name, so a user's CronJob can
	// collide with it. Only the one adept created may be deleted.
	theirs := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "maintenance", Namespace: "svc"},
		Spec:       batchv1.CronJobSpec{Schedule: "0 0 * * *"},
	}

	c, _, err := reconcileMaintenance(t,
		namedWindow("sunday-night", false), maintenance("sunday-night"), theirs)
	require.NoError(t, err)

	assert.Equal(t, "0 0 * * *", getCronJob(t, c).Spec.Schedule,
		"someone else's CronJob is not adept's to delete")
}

func TestMaintenance_LeavesTheVersionControllersStatusAlone(t *testing.T) {
	// Both controllers write this object's status under their own field
	// manager. A field this one does not carry through reads as a difference on
	// every reconcile, and the status gets reapplied forever.
	md := maintenance("sunday-night")
	md.Status.VersionUpdatedFor = ptr.To(metav1.NewTime(time.Now().Truncate(time.Second)))
	md.Status.ObservedBumpRequest = "now"

	m, c, _ := maintenanceManager(namedWindow("sunday-night", false), maintenanceRitual("svc"), md)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "maintenance", Namespace: "svc"}}

	_, err := m.Reconcile(context.Background(), req)
	require.NoError(t, err)

	settled := getMaintenance(t, c)
	assert.Equal(t, "now", settled.Status.ObservedBumpRequest)
	require.NotNil(t, settled.Status.VersionUpdatedFor)

	_, err = m.Reconcile(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, settled.ResourceVersion, getMaintenance(t, c).ResourceVersion,
		"an unchanged status must not be written again")
}

func TestMaintenanceWindowMapFunc_MatchesByNameAndByDefault(t *testing.T) {
	// A window edit must reach every definition naming it AND every definition
	// with an empty window when that window is the default. Matching only by
	// name leaves defaulted instances on the stale schedule with nothing to
	// say so.
	byName := maintenance("sunday-night")
	byName.Name, byName.Namespace = "named", "a"
	byDefault := maintenance("")
	byDefault.Name, byDefault.Namespace = "defaulted", "b"
	unrelated := maintenance("weekend")
	unrelated.Name, unrelated.Namespace = "other", "c"

	m, _, _ := maintenanceManager(byName, byDefault, unrelated)

	got := m.MaintenanceWindowMapFunc(context.Background(), namedWindow("sunday-night", true))

	var names []string
	for _, r := range got {
		names = append(names, r.Name)
	}
	assert.ElementsMatch(t, []string{"named", "defaulted"}, names)
}
