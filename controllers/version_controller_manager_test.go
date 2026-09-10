package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func versionManager(objs ...client.Object) (*VersionManager, client.Client) {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ritualsv1.Maintenance{}).
		WithObjects(objs...).
		Build()
	return &VersionManager{Client: c, Scheme: scheme,
		Recorder: events.NewFakeRecorder(8), Log: logr.Discard()}, c
}

func reconcileVersion(t *testing.T, m *VersionManager) ctrl.Result {
	t.Helper()
	res, err := m.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maintenance", Namespace: "svc"},
	})
	require.NoError(t, err)
	return res
}

func TestVersion_RequeuesForTheNextBump(t *testing.T) {
	// The bump has no CronJob firing it, so this controller has to wake itself
	// up for it, a lead ahead of the maintenance it must precede.
	m, _ := versionManager(namedWindow("sunday-night", false), maintenance("sunday-night"))

	res := reconcileVersion(t, m)

	want, err := schedule.NextBump(namedWindow("sunday-night", false).Spec, "svc/maintenance", time.Now())
	require.NoError(t, err)

	assert.Positive(t, res.RequeueAfter, "a settled maintenance must still wake up for its bump")
	assert.InDelta(t, time.Until(want).Seconds(), res.RequeueAfter.Seconds(), 5)
}

func TestVersion_NeedsNoRitualDefinition(t *testing.T) {
	// A missing ritual stops the CronJob, not the version. Sharing a Reconcile
	// with MaintenanceManager would have blocked this on the same failure.
	m, _ := versionManager(namedWindow("sunday-night", false), maintenance("sunday-night"))

	assert.Positive(t, reconcileVersion(t, m).RequeueAfter)
}

func TestVersion_UnresolvableWindowIsRetried(t *testing.T) {
	m, _ := versionManager(maintenance("sunday-night"))

	_, err := m.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maintenance", Namespace: "svc"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sunday-night")
}

func TestVersion_MissingMaintenanceIsNotAnError(t *testing.T) {
	m, _ := versionManager()

	res, err := m.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maintenance", Namespace: "svc"},
	})
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter, "nothing to wake up for")
}

func at(t *testing.T, month, day, hour, minute int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Zurich")
	require.NoError(t, err)
	return time.Date(2026, time.Month(month), day, hour, minute, 0, 0, loc)
}

// bumpMoment is the instant this instance's controller would wake up: the lead
// before its next maintenance. Derived rather than written down, because the
// instance's offset decides it and a hand-picked clock time is only inside the
// lead by luck.
func bumpMoment(t *testing.T) time.Time {
	t.Helper()
	bump, err := schedule.NextBump(namedWindow("sunday-night", false).Spec,
		"svc/maintenance", at(t, 9, 13, 12, 0))
	require.NoError(t, err)
	return bump
}

func dueAt(t *testing.T, now time.Time) time.Time {
	t.Helper()
	occurrence, open, err := schedule.BumpDue(
		namedWindow("sunday-night", false).Spec, "svc/maintenance", now)
	require.NoError(t, err)
	require.True(t, open, "the fixture time must be inside the window")
	return occurrence
}

func bumpedMaintenance(occurrence time.Time) *ritualsv1.Maintenance {
	md := maintenance("sunday-night")
	md.Status.VersionUpdatedFor = ptr.To(metav1.NewTime(occurrence))
	return md
}

func getVersionMaintenance(t *testing.T, c client.Client) *ritualsv1.Maintenance {
	t.Helper()
	got := &ritualsv1.Maintenance{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "maintenance", Namespace: "svc"}, got))
	return got
}

func TestVersion_RecordsTheMaintenanceItActedOn(t *testing.T) {
	inWindow := bumpMoment(t)
	m, c := versionManager(namedWindow("sunday-night", false), maintenance("sunday-night"))
	m.Now = func() time.Time { return inWindow }

	reconcileVersion(t, m)

	got := getVersionMaintenance(t, c)
	require.NotNil(t, got.Status.VersionUpdatedFor)
	assert.True(t, got.Status.VersionUpdatedFor.Time.Equal(dueAt(t, inWindow)))
}

func TestVersion_DoesNotActTwiceForTheSameMaintenance(t *testing.T) {
	// The restart case: a re-list reconciles every instance, and the watermark
	// is the only thing stopping the whole fleet from moving again.
	inWindow := bumpMoment(t)
	m, c := versionManager(
		namedWindow("sunday-night", false), bumpedMaintenance(dueAt(t, inWindow)))
	m.Now = func() time.Time { return inWindow }

	before := getVersionMaintenance(t, c).ResourceVersion
	reconcileVersion(t, m)

	assert.Equal(t, before, getVersionMaintenance(t, c).ResourceVersion,
		"an already-acted-on maintenance must not be written again")
}

func TestVersion_DoesNotActOutsideTheWindow(t *testing.T) {
	m, c := versionManager(namedWindow("sunday-night", false), maintenance("sunday-night"))
	m.Now = func() time.Time { return at(t, 9, 16, 12, 0) }

	reconcileVersion(t, m)

	assert.Nil(t, getVersionMaintenance(t, c).Status.VersionUpdatedFor,
		"nothing may be recorded while the window is shut")
}

func TestVersion_ActsAgainForTheNextMaintenance(t *testing.T) {
	// A watermark from last week must not suppress this week.
	inWindow := bumpMoment(t)
	occurrence := dueAt(t, inWindow)
	m, c := versionManager(
		namedWindow("sunday-night", false), bumpedMaintenance(occurrence.AddDate(0, 0, -7)))
	m.Now = func() time.Time { return inWindow }

	reconcileVersion(t, m)

	got := getVersionMaintenance(t, c)
	require.NotNil(t, got.Status.VersionUpdatedFor)
	assert.True(t, got.Status.VersionUpdatedFor.Time.Equal(occurrence))
}

func TestVersion_CatchesUpAfterMissingTheLead(t *testing.T) {
	// The reason for a watermark rather than a clock check: a controller that
	// was down through the lead still acts, as long as the window is open.
	afterTheRun := bumpMoment(t).Add(20 * time.Minute)
	m, c := versionManager(namedWindow("sunday-night", false), maintenance("sunday-night"))
	m.Now = func() time.Time { return afterTheRun }

	reconcileVersion(t, m)

	got := getVersionMaintenance(t, c)
	require.NotNil(t, got.Status.VersionUpdatedFor)
	assert.True(t, got.Status.VersionUpdatedFor.Time.Equal(dueAt(t, afterTheRun)))
}
