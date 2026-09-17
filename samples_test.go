package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
	"github.com/helmetica-framework/adept/schedule"
)

// The samples under config/samples are what the README's quickstart tells
// people to apply, so a sample that no longer matches the API is a broken
// first impression. These tests live at the repo root so the paths are
// repo-relative, and they need no cluster.

const samplesDir = "config/samples"

func TestSamplesAreWiredIntoKustomization(t *testing.T) {
	// A sample nobody references is a sample nobody applies, and the omission
	// is silent: `kustomize build` succeeds either way.
	kustomization, err := os.ReadFile(filepath.Join(samplesDir, "kustomization.yaml"))
	require.NoError(t, err)

	entries, err := filepath.Glob(filepath.Join(samplesDir, "v1_*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, entries, "expected at least one sample")

	for _, entry := range entries {
		name := filepath.Base(entry)
		assert.Contains(t, string(kustomization), name,
			"%s is not listed in the samples kustomization", name)
	}
}

func TestSampleMaintenanceWindowIsValid(t *testing.T) {
	w := readSample[ritualsv1.MaintenanceWindow](t, "v1_maintenancewindow.yaml")

	assert.Equal(t, "MaintenanceWindow", w.Kind)
	assert.Equal(t, ritualsv1.GroupVersion.String(), w.APIVersion)
	assert.NotEmpty(t, w.Name)
	assert.Empty(t, w.Namespace, "MaintenanceWindow is cluster-scoped")

	require.NoError(t, schedule.Validate(w.Spec),
		"the shipped sample must satisfy the rules Validate enforces")
}

func TestSampleMaintenanceWindowRendersASchedule(t *testing.T) {
	// The sample is the worked example an operator copies, so it should be
	// something that actually produces a CronJob rather than merely parsing.
	w := readSample[ritualsv1.MaintenanceWindow](t, "v1_maintenancewindow.yaml")

	cron, tz, err := schedule.CronSchedule(w.Spec, "db-instance")
	require.NoError(t, err)

	assert.NotEmpty(t, tz)
	// Five space-separated cron fields: minute, hour, day-of-month, month,
	// day-of-week.
	assert.Len(t, strings.Fields(cron), 5, "got %q", cron)
}

func TestSampleMaintenanceIsValid(t *testing.T) {
	md := readSample[ritualsv1.Maintenance](t, "v1_maintenance.yaml")

	assert.Equal(t, "Maintenance", md.Kind)
	assert.Equal(t, ritualsv1.GroupVersion.String(), md.APIVersion)
	assert.NotEmpty(t, md.Name)
	assert.Empty(t, md.Spec.Ritual,
		"left unset so the sample shows an empty spec being a complete schedule")
}

func TestSampleMaintenanceNamesAWindowThatExists(t *testing.T) {
	// Naming a window nothing defines is the one way this sample can be applied
	// and still resolve to nothing.
	md := readSample[ritualsv1.Maintenance](t, "v1_maintenance.yaml")
	w := readSample[ritualsv1.MaintenanceWindow](t, "v1_maintenancewindow.yaml")

	assert.Equal(t, w.Name, md.Spec.Window)
}

func TestSampleMaintenanceResolvesToAShippedRitual(t *testing.T) {
	// The sample leaves spec.ritual empty, so what runs is the CRD's default.
	// Read it from the generated CRD rather than repeating it here: change the
	// default without shipping a Definition of that name and applying the
	// samples leaves the Maintenance reporting a missing ritual.
	ritual := maintenanceRitualDefault(t)

	var names []string
	entries, err := filepath.Glob(filepath.Join(samplesDir, "v1_definition*.yaml"))
	require.NoError(t, err)
	for _, entry := range entries {
		names = append(names, readSample[ritualsv1.Definition](t, filepath.Base(entry)).Name)
	}

	assert.Contains(t, names, ritual, "no sample Definition is named %q", ritual)
}

// maintenanceRitualDefault is the default the generated CRD applies to
// spec.ritual.
func maintenanceRitualDefault(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile("config/crd/bases/rituals.helmetica.io_maintenances.yaml")
	require.NoError(t, err)

	var crd apiextv1.CustomResourceDefinition
	require.NoError(t, k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(&crd))
	require.NotEmpty(t, crd.Spec.Versions)

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	ritual, ok := spec.Properties["ritual"]
	require.True(t, ok, "the Maintenance CRD has no spec.ritual")
	require.NotNil(t, ritual.Default, "spec.ritual has no default")

	var out string
	require.NoError(t, json.Unmarshal(ritual.Default.Raw, &out))
	return out
}

func TestSampleDefinitionAndActionStillParse(t *testing.T) {
	// The existing samples predate these tests. Included so the guard covers
	// every sample rather than only the new one.
	def := readSample[ritualsv1.Definition](t, "v1_definition.yaml")
	assert.Equal(t, "Definition", def.Kind)
	assert.NotEmpty(t, def.Spec.JobTemplate.Spec.Template.Spec.Containers)

	act := readSample[ritualsv1.Action](t, "v1_action.yaml")
	assert.Equal(t, "Action", act.Kind)
	assert.NotEmpty(t, act.Spec.Type)
}

// readSample decodes one sample manifest into T.
func readSample[T any](t *testing.T, name string) T {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(samplesDir, name))
	require.NoError(t, err, "sample %s must exist", name)

	var out T
	require.NoError(t, k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(&out),
		"sample %s must decode into the typed API", name)
	return out
}
