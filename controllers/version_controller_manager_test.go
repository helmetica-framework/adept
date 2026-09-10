package controllers

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
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

	// The claim's status is a subresource on a real cluster, and the fake client
	// only routes Status() writes for kinds it has been told about. Without this
	// an apply to the claim's status comes back as a not-found.
	claimKind := &unstructured.Unstructured{}
	claimKind.SetGroupVersionKind(databaseRef().GVK)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&ritualsv1.Maintenance{}, claimKind).
		WithObjects(objs...).
		Build()
	return &VersionManager{Client: c, Scheme: scheme,
		Recorder: events.NewFakeRecorder(8), Log: logr.Discard()}, c
}

// managedInstance is what chrysopoeia leaves behind for an instance: an
// annotated namespace, the claim those annotations point at, and the generated
// CRD the newest version is read from.
func managedInstance() []client.Object {
	return []client.Object{
		svcNamespace(claimAnnotations()),
		claim("tenant", "svc"),
		databaseCRD("2.1.0", "2.0.0"),
	}
}

// managedVersionManager seeds a manager whose Maintenance belongs to a managed
// instance, which is what every Reconcile-level test needs: without the
// namespace and the claim there is nothing to bump.
func managedVersionManager(objs ...client.Object) (*VersionManager, client.Client) {
	return versionManager(append(managedInstance(), objs...)...)
}

// claimVersion is the claim's status.version, or "" when it has none.
func claimVersion(t *testing.T, c client.Client) string {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(databaseRef().GVK)
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "my-claim", Namespace: "tenant"}, got))
	version, _, err := unstructured.NestedString(got.Object, "status", "version")
	require.NoError(t, err)
	return version
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
	m, _ := managedVersionManager(namedWindow("sunday-night", false), maintenance("sunday-night"))

	res := reconcileVersion(t, m)

	want, err := schedule.NextBump(namedWindow("sunday-night", false).Spec, "svc/maintenance", time.Now())
	require.NoError(t, err)

	assert.Positive(t, res.RequeueAfter, "a settled maintenance must still wake up for its bump")
	assert.InDelta(t, time.Until(want).Seconds(), res.RequeueAfter.Seconds(), 5)
}

func TestVersion_NeedsNoRitualDefinition(t *testing.T) {
	// A missing ritual stops the CronJob, not the version. Sharing a Reconcile
	// with MaintenanceManager would have blocked this on the same failure.
	m, _ := managedVersionManager(namedWindow("sunday-night", false), maintenance("sunday-night"))

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
	m, c := managedVersionManager(namedWindow("sunday-night", false), maintenance("sunday-night"))
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
	m, c := managedVersionManager(
		namedWindow("sunday-night", false), bumpedMaintenance(dueAt(t, inWindow)))
	m.Now = func() time.Time { return inWindow }

	before := getVersionMaintenance(t, c).ResourceVersion
	reconcileVersion(t, m)

	assert.Equal(t, before, getVersionMaintenance(t, c).ResourceVersion,
		"an already-acted-on maintenance must not be written again")
}

func TestVersion_DoesNotActOutsideTheWindow(t *testing.T) {
	m, c := managedVersionManager(namedWindow("sunday-night", false), maintenance("sunday-night"))
	m.Now = func() time.Time { return at(t, 9, 16, 12, 0) }

	reconcileVersion(t, m)

	assert.Nil(t, getVersionMaintenance(t, c).Status.VersionUpdatedFor,
		"nothing may be recorded while the window is shut")
}

func TestVersion_ActsAgainForTheNextMaintenance(t *testing.T) {
	// A watermark from last week must not suppress this week.
	inWindow := bumpMoment(t)
	occurrence := dueAt(t, inWindow)
	m, c := managedVersionManager(
		namedWindow("sunday-night", false), bumpedMaintenance(occurrence.AddDate(0, 0, -7)))
	m.Now = func() time.Time { return inWindow }

	reconcileVersion(t, m)

	got := getVersionMaintenance(t, c)
	require.NotNil(t, got.Status.VersionUpdatedFor)
	assert.True(t, got.Status.VersionUpdatedFor.Time.Equal(occurrence))
}

// svcNamespace is the instance namespace a Maintenance lives in. chrysopoeia
// annotates it with the claim it was rendered for.
func svcNamespace(annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Annotations: annotations},
	}
}

func claimAnnotations() map[string]string {
	return map[string]string{
		claimAPIVersionAnnotation: "example.org/v1",
		claimKindAnnotation:       "Database",
		claimNamespaceAnnotation:  "tenant",
		claimNameAnnotation:       "my-claim",
	}
}

func TestVersion_ClaimRefComesFromTheNamespaceAnnotations(t *testing.T) {
	// The claim lives in the user's namespace, not the instance's, so the ref
	// has to carry a namespace of its own rather than reusing the Maintenance's.
	m, _ := versionManager(svcNamespace(claimAnnotations()))

	got, managed, err := m.claimRefFor(context.Background(), "svc")

	require.NoError(t, err)
	require.True(t, managed)
	assert.Equal(t, claimRef{
		GVK:       schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "Database"},
		Namespace: "tenant",
		Name:      "my-claim",
	}, got)
}

func TestVersion_MissingNamespaceStaysNotFound(t *testing.T) {
	// Callers distinguish "the namespace went away mid-reconcile" from a real
	// failure, so the wrapping must keep IsNotFound working.
	m, _ := versionManager()

	_, _, err := m.claimRefFor(context.Background(), "svc")

	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err), "got %v", err)
}

func TestVersion_UnannotatedNamespaceIsUnmanaged(t *testing.T) {
	// A chart installed with plain helm has no chrysopoeia annotations. There
	// is no claim to bump and nothing is wrong.
	m, _ := versionManager(svcNamespace(nil))

	_, managed, err := m.claimRefFor(context.Background(), "svc")

	require.NoError(t, err)
	assert.False(t, managed)
}

func TestVersion_PartialAnnotationsNameTheMissingOne(t *testing.T) {
	// chrysopoeia writes the set in one apply, so half of it is a bug rather
	// than an unmanaged namespace.
	// claim-kind rather than claim-name: "chrysopoeia.io/claim-name" is a
	// substring of "chrysopoeia.io/claim-namespace", so a message that listed
	// the annotations it found would satisfy the assertion without ever naming
	// the missing one.
	annotations := claimAnnotations()
	delete(annotations, claimKindAnnotation)
	m, _ := versionManager(svcNamespace(annotations))

	_, _, err := m.claimRefFor(context.Background(), "svc")

	require.Error(t, err)
	assert.Contains(t, err.Error(), claimKindAnnotation)
}

func TestVersion_UnparseableClaimAPIVersionIsAnError(t *testing.T) {
	annotations := claimAnnotations()
	annotations[claimAPIVersionAnnotation] = "example.org/v1/extra"
	m, _ := versionManager(svcNamespace(annotations))

	_, _, err := m.claimRefFor(context.Background(), "svc")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "example.org/v1/extra")
}

func databaseRef() claimRef {
	return claimRef{
		GVK:       schema.GroupVersionKind{Group: "example.org", Version: "v1", Kind: "Database"},
		Namespace: "tenant",
		Name:      "my-claim",
	}
}

func TestVersion_ClaimComesFromTheRefsOwnNamespace(t *testing.T) {
	// The claim is the user's object, in the user's namespace. A same-named one
	// in the instance namespace must not be picked up instead.
	m, _ := versionManager(claim("tenant", "svc"), claim("svc", "svc"))

	got, err := m.claimFor(context.Background(), databaseRef())

	require.NoError(t, err)
	assert.Equal(t, "tenant", got.GetNamespace())
	assert.Equal(t, "my-claim", got.GetName())
}

func TestVersion_MissingClaimStaysNotFound(t *testing.T) {
	// The claim may not exist yet, so the caller has to be able to tell a
	// retryable absence from a real failure.
	m, _ := versionManager()

	_, err := m.claimFor(context.Background(), databaseRef())

	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err), "got %v", err)
	assert.Contains(t, err.Error(), "my-claim")
}

func TestVersion_ClaimKindMustMatchTheRef(t *testing.T) {
	// A Database at that name and namespace is not the Vault the ref asks for.
	// Catches an implementation that gets the object but ignores the ref's GVK.
	ref := databaseRef()
	ref.GVK.Kind = "Vault"
	m, _ := versionManager(claim("tenant", "svc"))

	_, err := m.claimFor(context.Background(), ref)

	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err), "got %v", err)
}

// databaseCRD is the CRD chrysopoeia generates for the Database claim kind.
// versions is the spec.version enum, which it writes newest first.
func databaseCRD(versions ...string) *apiextv1.CustomResourceDefinition {
	enum := make([]apiextv1.JSON, len(versions))
	for i, v := range versions {
		enum[i] = apiextv1.JSON{Raw: []byte(strconv.Quote(v))}
	}

	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "databases.example.org",
			Labels: map[string]string{ManagedLabel: ""},
		},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: "example.org",
			Names: apiextv1.CustomResourceDefinitionNames{Kind: "Database", Plural: "databases"},
			Versions: []apiextv1.CustomResourceDefinitionVersion{{
				Name: "v1",
				Schema: &apiextv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextv1.JSONSchemaProps{
							"spec": {
								Type: "object",
								Properties: map[string]apiextv1.JSONSchemaProps{
									"version": {Type: "string", Enum: enum},
								},
							},
						},
					},
				},
			}},
		},
	}
}

// otherCRD is a second CRD in the cluster, to prove the lookup matches rather
// than takes whatever it finds first.
func otherCRD() *apiextv1.CustomResourceDefinition {
	crd := databaseCRD("9.9.9")
	crd.Name = "vaults.other.org"
	crd.Spec.Group = "other.org"
	crd.Spec.Names = apiextv1.CustomResourceDefinitionNames{Kind: "Vault", Plural: "vaults"}
	return crd
}

func TestVersion_ClaimCRDIsMatchedByGroupAndKind(t *testing.T) {
	m, _ := versionManager(otherCRD(), databaseCRD("2.0.0"))

	got, err := m.claimCRDFor(context.Background(), databaseRef())

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "databases.example.org", got.GetName())
}

func TestVersion_ACRDChrysopoeiaDoesNotManageIsIgnored(t *testing.T) {
	// Same group and kind, but not one of the generated claim CRDs. Reading a
	// version enum off it would be reading someone else's schema.
	unmanaged := databaseCRD("9.9.9")
	unmanaged.Labels = nil
	m, _ := versionManager(unmanaged)

	_, err := m.claimCRDFor(context.Background(), databaseRef())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Database")
}

func TestVersion_NoCRDForTheClaimKindIsAnError(t *testing.T) {
	// The namespace annotations can outlive the reagent that produced them.
	m, _ := versionManager(otherCRD())

	_, err := m.claimCRDFor(context.Background(), databaseRef())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Database")
}

func TestVersion_NewestVersionIsTheFirstEnumEntry(t *testing.T) {
	// Chrysopoeia sorts descending, so first is newest and adept does not sort.
	got, err := newestVersion(databaseCRD("2.1.0", "2.0.0", "1.9.0"), databaseRef())

	require.NoError(t, err)
	assert.Equal(t, "2.1.0", got)
}

func TestVersion_NewestVersionUnquotesTheEnumEntry(t *testing.T) {
	// The entries are raw JSON. string(Raw) would hand the claim `"2.1.0"`,
	// quotes included, and the schema would reject it.
	got, err := newestVersion(databaseCRD("2.1.0"), databaseRef())

	require.NoError(t, err)
	assert.Equal(t, "2.1.0", got)
	assert.NotContains(t, got, `"`)
}

func TestVersion_NewestVersionNeedsTheRefsVersion(t *testing.T) {
	// A claim served at v1 must not be answered from a v1beta1 schema.
	crd := databaseCRD("2.1.0")
	crd.Spec.Versions[0].Name = "v1beta1"

	_, err := newestVersion(crd, databaseRef())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "v1")
}

func TestVersion_AnEmptyEnumIsAnError(t *testing.T) {
	// Not an empty string: a claim kind with nothing to select means discovery
	// found no tags, and bumping to "" would clear the user's version.
	_, err := newestVersion(databaseCRD(), databaseRef())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Database", "the message has to say which claim kind is broken")
}

func TestVersion_ASchemaWithoutAVersionPropertyIsAnError(t *testing.T) {
	// Then it is not a CRD chrysopoeia generated, and the annotations lied.
	crd := databaseCRD("2.1.0")
	delete(crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties, "version")

	_, err := newestVersion(crd, databaseRef())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "version")
}

// claimWithSpecVersion is a claim carrying a spec.version of any type, so the
// non-string case can be built too.
func claimWithSpecVersion(version any) *unstructured.Unstructured {
	c := claim("tenant", "svc")
	utilruntime.Must(unstructured.SetNestedField(c.Object, version, "spec", "version"))
	return c
}

func TestVersion_PinnedVersionIsWhatTheUserSet(t *testing.T) {
	got, err := pinnedVersion(claimWithSpecVersion("1.2.3"))

	require.NoError(t, err)
	assert.Equal(t, "1.2.3", got)
}

func TestVersion_AClaimWithoutASpecVersionIsManaged(t *testing.T) {
	// Absent is the normal state of an instance the framework owns, once
	// chrysopoeia stops defaulting the field.
	got, err := pinnedVersion(claim("tenant", "svc"))

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestVersion_AnEmptySpecVersionIsManaged(t *testing.T) {
	// Present but empty has to read the same as absent, or every instance a
	// user has ever touched looks pinned.
	got, err := pinnedVersion(claimWithSpecVersion(""))

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestVersion_ANonStringSpecVersionIsAnError(t *testing.T) {
	// Reading it as unpinned would bump a field we have misunderstood.
	_, err := pinnedVersion(claimWithSpecVersion(int64(3)))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "version", "the message has to say which field is the wrong shape")
}

// withStatusVersion puts a status.version of any type on a claim, so the
// non-string case can be built too.
func withStatusVersion(c *unstructured.Unstructured, version any) *unstructured.Unstructured {
	utilruntime.Must(unstructured.SetNestedField(c.Object, version, "status", "version"))
	return c
}

func TestVersion_AClaimAlreadyOnTheNewestIsNotBumped(t *testing.T) {
	// The no-churn case: this runs on every reconcile, and each write would be
	// a revision downstream.
	got, err := needsBump(withStatusVersion(claim("tenant", "svc"), "2.1.0"), "2.1.0")

	require.NoError(t, err)
	assert.False(t, got)
}

func TestVersion_AClaimOnAnOlderVersionIsBumped(t *testing.T) {
	got, err := needsBump(withStatusVersion(claim("tenant", "svc"), "2.0.0"), "2.1.0")

	require.NoError(t, err)
	assert.True(t, got)
}

func TestVersion_AClaimWithNoStatusVersionIsBumped(t *testing.T) {
	// The first bump an instance ever gets.
	got, err := needsBump(claim("tenant", "svc"), "2.1.0")

	require.NoError(t, err)
	assert.True(t, got)
}

func TestVersion_APinnedClaimIsNeverBumped(t *testing.T) {
	// Pinned wins over a stale status.version, and the stale one is left as it
	// is: resolution reads spec.version first, so nothing downstream sees it.
	pinned := withStatusVersion(claimWithSpecVersion("1.0.0"), "2.0.0")

	got, err := needsBump(pinned, "2.1.0")

	require.NoError(t, err)
	assert.False(t, got)
}

func TestVersion_ANonStringStatusVersionIsAnError(t *testing.T) {
	_, err := needsBump(withStatusVersion(claim("tenant", "svc"), int64(3)), "2.1.0")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "version", "the message has to say which field is the wrong shape")
}

func TestVersion_BumpClaimWritesTheVersion(t *testing.T) {
	m, c := versionManager(claim("tenant", "svc"))

	require.NoError(t, m.bumpClaim(context.Background(), claim("tenant", "svc"), "2.1.0"))

	assert.Equal(t, "2.1.0", claimVersion(t, c))
}

func TestVersion_BumpClaimLeavesTheRestOfTheStatusAlone(t *testing.T) {
	// status.instanceNamespace is chrysopoeia's, and everything downstream
	// resolves the instance through it. A write that replaces the status object
	// rather than one field takes it out.
	m, c := versionManager(claim("tenant", "svc"))

	require.NoError(t, m.bumpClaim(context.Background(), claim("tenant", "svc"), "2.1.0"))

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(databaseRef().GVK)
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "my-claim", Namespace: "tenant"}, got))
	instanceNs, _, err := unstructured.NestedString(got.Object, "status", "instanceNamespace")
	require.NoError(t, err)
	assert.Equal(t, "svc", instanceNs)
}

func TestVersion_ReconcileBumpsTheClaim(t *testing.T) {
	inWindow := bumpMoment(t)
	m, c := managedVersionManager(namedWindow("sunday-night", false), maintenance("sunday-night"))
	m.Now = func() time.Time { return inWindow }

	reconcileVersion(t, m)

	assert.Equal(t, "2.1.0", claimVersion(t, c), "the newest the claim's CRD allows")
	require.NotNil(t, getVersionMaintenance(t, c).Status.VersionUpdatedFor)
}

func TestVersion_ReconcileSkipsAnUnmanagedNamespace(t *testing.T) {
	// Installed with plain helm. Nothing to bump, and nothing wrong either, so
	// no watermark: it would claim a version was written.
	inWindow := bumpMoment(t)
	m, c := versionManager(svcNamespace(nil), namedWindow("sunday-night", false), maintenance("sunday-night"))
	m.Now = func() time.Time { return inWindow }

	res := reconcileVersion(t, m)

	assert.Nil(t, getVersionMaintenance(t, c).Status.VersionUpdatedFor)
	assert.Positive(t, res.RequeueAfter, "it still has to wake up for the next one")
}

func TestVersion_ReconcileLeavesAPinnedClaimAlone(t *testing.T) {
	inWindow := bumpMoment(t)
	pinned := claimWithSpecVersion("1.0.0")
	m, c := versionManager(svcNamespace(claimAnnotations()), pinned, databaseCRD("2.1.0"),
		namedWindow("sunday-night", false), maintenance("sunday-night"))
	m.Now = func() time.Time { return inWindow }

	reconcileVersion(t, m)

	assert.Empty(t, claimVersion(t, c), "pinned means the framework does not choose")
	assert.Nil(t, getVersionMaintenance(t, c).Status.VersionUpdatedFor,
		"nothing was written, so nothing to watermark")
}

func TestVersion_ReconcileReportsAnUnresolvableClaim(t *testing.T) {
	// The annotations point at a claim that is not there, which is retryable
	// rather than something to skip.
	inWindow := bumpMoment(t)
	m, _ := versionManager(svcNamespace(claimAnnotations()), databaseCRD("2.1.0"),
		namedWindow("sunday-night", false), maintenance("sunday-night"))
	m.Now = func() time.Time { return inWindow }

	_, err := m.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maintenance", Namespace: "svc"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "my-claim")
}

func TestVersion_CatchesUpAfterMissingTheLead(t *testing.T) {
	// The reason for a watermark rather than a clock check: a controller that
	// was down through the lead still acts, as long as the window is open.
	afterTheRun := bumpMoment(t).Add(20 * time.Minute)
	m, c := managedVersionManager(namedWindow("sunday-night", false), maintenance("sunday-night"))
	m.Now = func() time.Time { return afterTheRun }

	reconcileVersion(t, m)

	got := getVersionMaintenance(t, c)
	require.NotNil(t, got.Status.VersionUpdatedFor)
	assert.True(t, got.Status.VersionUpdatedFor.Time.Equal(dueAt(t, afterTheRun)))
}
