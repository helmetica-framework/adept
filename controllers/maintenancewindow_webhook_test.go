package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
)

// These tests cover the wiring, not the rules. What counts as a valid window
// is schedule.Validate's business and is exercised exhaustively in that
// package; duplicating the table here would mean two places to update every
// time a rule changes.

func maintenanceWindow() *ritualsv1.MaintenanceWindow {
	return &ritualsv1.MaintenanceWindow{
		ObjectMeta: metav1.ObjectMeta{Name: "sunday-night"},
		Spec: ritualsv1.MaintenanceWindowSpec{
			DaysOfWeek: []ritualsv1.Day{ritualsv1.Sunday},
			Time:       "22:00",
			Duration:   metav1.Duration{Duration: 6 * time.Hour},
			TimeZone:   "Europe/Zurich",
		},
	}
}

func TestMaintenanceWindowValidator_AcceptsAValidWindow(t *testing.T) {
	v := &MaintenanceWindowValidator{}

	warnings, err := v.ValidateCreate(context.Background(), maintenanceWindow())
	require.NoError(t, err)
	assert.Empty(t, warnings, "a valid window should be admitted silently")
}

func TestMaintenanceWindowValidator_RejectsOnCreate(t *testing.T) {
	v := &MaintenanceWindowValidator{}

	w := maintenanceWindow()
	w.Spec.TimeZone = "Europe/Zurizh"

	_, err := v.ValidateCreate(context.Background(), w)
	// The time zone case specifically: it is the one rule the CRD schema
	// cannot express, and the reason this webhook exists at all.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Europe/Zurizh")
}

func TestMaintenanceWindowValidator_RejectsOnUpdate(t *testing.T) {
	v := &MaintenanceWindowValidator{}

	old := maintenanceWindow()
	updated := maintenanceWindow()
	updated.Spec.Time = "25:00"

	_, err := v.ValidateUpdate(context.Background(), old, updated)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "25:00")
}

func TestMaintenanceWindowValidator_UpdateJudgesTheNewObject(t *testing.T) {
	// An edit that repairs a window must be admitted, even though the object
	// being replaced is invalid. Windows stored before this webhook existed
	// are exactly that case, so getting the argument order wrong would leave
	// them unfixable.
	v := &MaintenanceWindowValidator{}

	old := maintenanceWindow()
	old.Spec.Time = "25:00"

	warnings, err := v.ValidateUpdate(context.Background(), old, maintenanceWindow())
	require.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestMaintenanceWindowValidator_AllowsDelete(t *testing.T) {
	// Deletion is not in the webhook's verbs, but the interface requires the
	// method. It must never block a delete: an invalid window has to be
	// removable, and one stored before this webhook existed may well be
	// invalid.
	v := &MaintenanceWindowValidator{}

	w := maintenanceWindow()
	w.Spec.TimeZone = "Europe/Zurizh"

	warnings, err := v.ValidateDelete(context.Background(), w)
	require.NoError(t, err)
	assert.Empty(t, warnings)
}

// There is deliberately no test for being handed some other kind.
// admission.Validator is generic over the type, so the methods take a
// *MaintenanceWindow and passing anything else does not compile. A runtime
// check would be strictly worse than the compiler's.
