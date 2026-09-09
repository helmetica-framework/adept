package schedule_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
	"github.com/helmetica-framework/adept/schedule"
)

// window is a six hour span opening at 22:00, in the given zone.
func window(tz string, days ...ritualsv1.Day) ritualsv1.MaintenanceWindowSpec {
	return ritualsv1.MaintenanceWindowSpec{
		DaysOfWeek: days,
		Time:       "22:00",
		Duration:   metav1.Duration{Duration: 6 * time.Hour},
		TimeZone:   tz,
	}
}

func TestEmbeddedTimeZoneDatabaseIsAvailable(t *testing.T) {
	// Guards the blank time/tzdata import. The manager image is
	// distroless-static, which ships no /usr/share/zoneinfo, so without the
	// import every non-UTC window works on a developer machine and fails in
	// the cluster. This test passes either way locally, so it is a reminder
	// rather than a trap, but it fails loudly in a scratch container.
	_, err := time.LoadLocation("Europe/Zurich")
	require.NoError(t, err, "Europe/Zurich must resolve without system zoneinfo")
}

func TestValidate_AcceptsAWellFormedWindow(t *testing.T) {
	assert.NoError(t, schedule.Validate(window("Europe/Zurich", "sunday")))
	assert.NoError(t, schedule.Validate(window("", "sunday")),
		"an empty time zone means UTC, not an error")
}

func TestValidate_RejectsMalformedSpecs(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*ritualsv1.MaintenanceWindowSpec)
	}{
		{"no days", func(s *ritualsv1.MaintenanceWindowSpec) { s.DaysOfWeek = nil }},
		{"unknown day", func(s *ritualsv1.MaintenanceWindowSpec) {
			s.DaysOfWeek = []ritualsv1.Day{"caturday"}
		}},
		{"unknown day alongside a valid one", func(s *ritualsv1.MaintenanceWindowSpec) {
			// Rejecting this needs every day checked, not just one match.
			s.DaysOfWeek = []ritualsv1.Day{"sunday", "caturday"}
		}},
		{"duplicate days", func(s *ritualsv1.MaintenanceWindowSpec) {
			s.DaysOfWeek = []ritualsv1.Day{"sunday", "sunday"}
		}},
		{"time out of range", func(s *ritualsv1.MaintenanceWindowSpec) { s.Time = "25:00" }},
		{"time not HH:MM", func(s *ritualsv1.MaintenanceWindowSpec) { s.Time = "10pm" }},
		{"empty time", func(s *ritualsv1.MaintenanceWindowSpec) { s.Time = "" }},
		{"zero duration", func(s *ritualsv1.MaintenanceWindowSpec) {
			s.Duration = metav1.Duration{}
		}},
		{"negative duration", func(s *ritualsv1.MaintenanceWindowSpec) {
			s.Duration = metav1.Duration{Duration: -time.Hour}
		}},
		{"duration below the floor", func(s *ritualsv1.MaintenanceWindowSpec) {
			s.Duration = metav1.Duration{Duration: 30 * time.Minute}
		}},
		{"duration above the ceiling", func(s *ritualsv1.MaintenanceWindowSpec) {
			s.Duration = metav1.Duration{Duration: 25 * time.Hour}
		}},
		{"unknown time zone", func(s *ritualsv1.MaintenanceWindowSpec) {
			s.TimeZone = "Europe/Zurizh"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := window("Europe/Zurich", "sunday")
			tt.mut(&spec)

			// The CRD schema catches unknown day names and an absent
			// daysOfWeek, but nothing else here: duplicates, the time format
			// and the duration bounds are this function's job alone.
			assert.Error(t, schedule.Validate(spec))
		})
	}
}

func TestValidate_AcceptsTheDurationBounds(t *testing.T) {
	for _, d := range []time.Duration{time.Hour, 24 * time.Hour} {
		spec := window("UTC", "sunday")
		spec.Duration = metav1.Duration{Duration: d}
		assert.NoError(t, schedule.Validate(spec), "%v is in range", d)
	}
}

func TestCronSchedule(t *testing.T) {
	tests := []struct {
		name         string
		days         []ritualsv1.Day
		time         string
		wantSchedule string
	}{
		{
			name:         "single day, no midnight crossing",
			days:         []ritualsv1.Day{"sunday"},
			time:         "22:00",
			wantSchedule: "19 23 * * 0", // 22:00 + db-prod's 1h19m offset
		},
		{
			name:         "a multi-day window",
			days:         []ritualsv1.Day{"sunday", "monday", "tuesday", "wednesday", "thursday"},
			time:         "22:00",
			wantSchedule: "19 23 * * 0,1,2,3,4",
		},
		{
			name: "offset crosses midnight and shifts the day",
			days: []ritualsv1.Day{"sunday"},
			time: "23:00",
			// 23:00 + 1h19m is Monday 00:19, not Sunday. Emitting
			// "19 23 * * 0" here would run every instance a day early.
			wantSchedule: "19 0 * * 1",
		},
		{
			name:         "saturday wraps around to sunday",
			days:         []ritualsv1.Day{"saturday"},
			time:         "23:00",
			wantSchedule: "19 0 * * 0",
		},
		{
			name: "day order in the spec does not change the output",
			days: []ritualsv1.Day{"thursday", "sunday", "tuesday"},
			time: "22:00",
			// Unsorted output would rewrite the CronJob on every reconcile.
			wantSchedule: "19 23 * * 0,2,4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := window("Europe/Zurich", tt.days...)
			spec.Time = tt.time

			sched, tz, err := schedule.CronSchedule(spec, "db-prod")
			require.NoError(t, err)
			assert.Equal(t, tt.wantSchedule, sched)
			assert.Equal(t, "Europe/Zurich", tz)
		})
	}
}

func TestCronSchedule_EmptyTimeZoneBecomesUTC(t *testing.T) {
	// The CRD defaults timeZone to UTC, but a spec built in Go bypasses
	// defaulting. CronJob.spec.timeZone must never be emitted empty.
	sched, tz, err := schedule.CronSchedule(window("", "sunday"), "db-prod")
	require.NoError(t, err)
	assert.Equal(t, "UTC", tz)
	assert.Equal(t, "19 23 * * 0", sched)
}

func TestCronSchedule_RejectsMalformedSpecs(t *testing.T) {
	spec := window("Europe/Zurich", "sunday")
	spec.TimeZone = "Europe/Zurizh"

	_, _, err := schedule.CronSchedule(spec, "db-prod")
	assert.Error(t, err)
}

func TestOffset_IsDeterministicAndBounded(t *testing.T) {
	spec := window("UTC", "sunday")
	span := 6 * time.Hour // the whole window: starts may land anywhere in it

	seen := map[time.Duration]int{}
	for i := range 200 {
		id := fmt.Sprintf("instance-%d", i)

		off := schedule.Offset(spec, id)
		assert.GreaterOrEqual(t, off, time.Duration(0))
		assert.Less(t, off, span, "a start must land inside the span")
		assert.Zero(t, off%time.Minute, "offset must be whole minutes")

		assert.Equal(t, off, schedule.Offset(spec, id), "offset must be stable across calls")

		seen[off]++
	}

	// 200 identities over a 360 minute span should land on well over 100
	// distinct minutes. A hash that collapses would fail here.
	assert.Greater(t, len(seen), 100, "identities must spread across the span")
}

func TestOffset_AtTheDurationFloor(t *testing.T) {
	// 1h is the narrowest window allowed, giving a 60 minute span. This is
	// where spreading is tightest and most likely to degenerate.
	spec := window("UTC", "sunday")
	spec.Duration = metav1.Duration{Duration: time.Hour}

	seen := map[time.Duration]int{}
	for i := range 100 {
		off := schedule.Offset(spec, fmt.Sprintf("instance-%d", i))
		assert.Less(t, off, time.Hour)
		seen[off]++
	}
	assert.Greater(t, len(seen), 20, "100 identities must not collapse onto a few minutes")
}

func TestOffset_ZeroDurationDoesNotPanic(t *testing.T) {
	// Validate rejects this, but Offset must not divide by zero if it is ever
	// called first.
	spec := window("UTC", "sunday")
	spec.Duration = metav1.Duration{}

	assert.NotPanics(t, func() {
		assert.Zero(t, schedule.Offset(spec, "db-prod"))
	})
}

func TestOffset_EmptyIdentityCollapses(t *testing.T) {
	// Every instance with a missing identity lands on the same minute. That is
	// acceptable (a missing identity is a caller bug, not an operator one) but
	// it is pinned here so it is a known property rather than a surprise.
	spec := window("UTC", "sunday")

	a := schedule.Offset(spec, "")
	assert.Equal(t, a, schedule.Offset(spec, ""))
	assert.Equal(t, time.Hour+17*time.Minute, a)
}

func TestOffset_StableAcrossProcesses(t *testing.T) {
	// Golden values pinning FNV-1a 64 over the identity, reduced modulo the
	// span in whole minutes. Changing the offset function reshuffles every
	// instance's maintenance start in every cluster, so that must be a
	// deliberate, visible change rather than a silent one. If this test fails,
	// the question is not "what are the new values" but "did we mean to move
	// every instance".
	spec := window("UTC", "sunday") // 6h span

	want := map[string]time.Duration{
		"db-prod":    1*time.Hour + 19*time.Minute,
		"db-staging": 4*time.Hour + 55*time.Minute,
		"cache-eu":   4*time.Hour + 22*time.Minute,
	}
	for id, w := range want {
		assert.Equal(t, w, schedule.Offset(spec, id), "offset(%q) moved", id)
	}
}
