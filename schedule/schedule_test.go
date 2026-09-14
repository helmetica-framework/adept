package schedule_test

import (
	"fmt"
	"strconv"
	"strings"
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

func zurich(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Zurich")
	require.NoError(t, err)
	return loc
}

func assertSameInstant(t *testing.T, want, got time.Time) {
	t.Helper()
	assert.True(t, want.Equal(got),
		"want %s, got %s", want.Format(time.RFC3339), got.Format(time.RFC3339))
}

// db-prod's offset in a 6h window is 1h19m, so a 22:00 window starts this
// instance at 23:19 and a 23:00 one at 00:19 the next day. 2026-09-13 is a
// Sunday.
func TestNextRun(t *testing.T) {
	loc := zurich(t)

	tests := []struct {
		name  string
		days  []ritualsv1.Day
		start string
		now   time.Time
		want  time.Time
	}{
		{
			name:  "later the same day",
			days:  []ritualsv1.Day{"sunday"},
			start: "22:00",
			now:   time.Date(2026, 9, 13, 12, 0, 0, 0, loc),
			want:  time.Date(2026, 9, 13, 23, 19, 0, 0, loc),
		},
		{
			name:  "standing on the start time takes the next one",
			days:  []ritualsv1.Day{"sunday"},
			start: "22:00",
			// Inclusive here would requeue with no delay and spin.
			now:  time.Date(2026, 9, 13, 23, 19, 0, 0, loc),
			want: time.Date(2026, 9, 20, 23, 19, 0, 0, loc),
		},
		{
			name:  "midweek waits for the next listed day",
			days:  []ritualsv1.Day{"sunday"},
			start: "22:00",
			now:   time.Date(2026, 9, 16, 9, 0, 0, 0, loc),
			want:  time.Date(2026, 9, 20, 23, 19, 0, 0, loc),
		},
		{
			name:  "an offset past midnight lands on the following day",
			days:  []ritualsv1.Day{"sunday"},
			start: "23:00",
			now:   time.Date(2026, 9, 13, 23, 30, 0, 0, loc),
			want:  time.Date(2026, 9, 14, 0, 19, 0, 0, loc),
		},
		{
			name:  "a multi-day window takes the nearest day",
			days:  []ritualsv1.Day{"sunday", "monday", "tuesday", "wednesday", "thursday"},
			start: "22:00",
			now:   time.Date(2026, 9, 14, 12, 0, 0, 0, loc),
			want:  time.Date(2026, 9, 14, 23, 19, 0, 0, loc),
		},
		{
			name:  "now in another zone is still judged in the window's",
			days:  []ritualsv1.Day{"sunday"},
			start: "22:00",
			// 21:00 UTC is 23:00 in Zurich, so the 23:19 start is still ahead.
			now:  time.Date(2026, 9, 13, 21, 0, 0, 0, time.UTC),
			want: time.Date(2026, 9, 13, 23, 19, 0, 0, loc),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := window("Europe/Zurich", tt.days...)
			spec.Time = tt.start

			got, err := schedule.NextRun(spec, "db-prod", tt.now)
			require.NoError(t, err)
			assertSameInstant(t, tt.want, got)
		})
	}
}

func TestNextRun_AgreesWithTheCronExpression(t *testing.T) {
	// The two must not drift: the CronJob fires on the expression while the
	// caller wakes on this time, and a mismatch means maintenance runs at one
	// moment and something acts on it at another.
	loc := zurich(t)
	now := time.Date(2026, 9, 16, 9, 0, 0, 0, loc)

	daySets := [][]ritualsv1.Day{
		{"sunday"},
		{"saturday"},
		{"sunday", "monday", "tuesday", "wednesday", "thursday"},
	}
	for _, days := range daySets {
		for _, start := range []string{"22:00", "23:00", "00:30"} {
			for _, id := range []string{"db-prod", "db-staging", "cache-eu"} {
				spec := window("Europe/Zurich", days...)
				spec.Time = start

				cron, _, err := schedule.CronSchedule(spec, id)
				require.NoError(t, err)
				got, err := schedule.NextRun(spec, id, now)
				require.NoError(t, err)

				wantFields := strings.Split(cron, " ")
				assert.Equal(t, wantFields[0], strconv.Itoa(got.Minute()), "minute of %q for %s", cron, id)
				assert.Equal(t, wantFields[1], strconv.Itoa(got.Hour()), "hour of %q for %s", cron, id)
				assert.Contains(t, strings.Split(wantFields[4], ","), strconv.Itoa(int(got.Weekday())),
					"weekday of %q for %s", cron, id)
				assert.True(t, got.After(now), "%s is not after %s", got, now)
			}
		}
	}
}

func TestNextRun_EmptyTimeZoneIsUTC(t *testing.T) {
	spec := window("", "sunday")
	spec.Time = "22:00"

	got, err := schedule.NextRun(spec, "db-prod", time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assertSameInstant(t, time.Date(2026, 9, 13, 23, 19, 0, 0, time.UTC), got)
}

func TestNextRun_RejectsMalformedSpecs(t *testing.T) {
	spec := window("Europe/Zurizh", "sunday")

	_, err := schedule.NextRun(spec, "db-prod", time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Europe/Zurizh")
}

func TestNextBump(t *testing.T) {
	loc := zurich(t)

	tests := []struct {
		name     string
		identity string
		now      time.Time
		want     time.Time
	}{
		{
			name:     "lands a lead ahead of the start",
			identity: "db-prod", // 1h19m offset, so 23:19
			now:      time.Date(2026, 9, 13, 12, 0, 0, 0, loc),
			want:     time.Date(2026, 9, 13, 23, 4, 0, 0, loc),
		},
		{
			name: "an offset shorter than the lead clamps to the opening",
			// svc/instance-52 has a 3m offset, so a full lead would put the
			// bump at 21:48, outside the window it belongs to.
			identity: "svc/instance-52",
			now:      time.Date(2026, 9, 13, 12, 0, 0, 0, loc),
			want:     time.Date(2026, 9, 13, 22, 0, 0, 0, loc),
		},
		{
			name:     "standing on the bump takes the next one",
			identity: "db-prod",
			now:      time.Date(2026, 9, 13, 23, 4, 0, 0, loc),
			want:     time.Date(2026, 9, 20, 23, 4, 0, 0, loc),
		},
		{
			name: "between the bump and the start takes the next one",
			// A restart here has missed this window's bump. Waiting is the
			// safe direction: acting now would be outside the lead the ritual
			// counts on.
			identity: "db-prod",
			now:      time.Date(2026, 9, 13, 23, 10, 0, 0, loc),
			want:     time.Date(2026, 9, 20, 23, 4, 0, 0, loc),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := window("Europe/Zurich", "sunday")

			got, err := schedule.NextBump(spec, tt.identity, tt.now)
			require.NoError(t, err)
			assertSameInstant(t, tt.want, got)
		})
	}
}

func TestNextBump_NeverAtOrAfterTheStart(t *testing.T) {
	// The whole point is ordering: whatever the offset, the bump has to be
	// strictly earlier than the maintenance it precedes, and no earlier than
	// the window opening.
	loc := zurich(t)
	now := time.Date(2026, 9, 16, 9, 0, 0, 0, loc)
	spec := window("Europe/Zurich", "sunday")

	for i := range 200 {
		id := fmt.Sprintf("svc/instance-%d", i)

		run, err := schedule.NextRun(spec, id, now)
		require.NoError(t, err)
		bump, err := schedule.NextBump(spec, id, now)
		require.NoError(t, err)

		offset := schedule.Offset(spec, id)
		if offset == 0 {
			assertSameInstant(t, run, bump)
			continue
		}
		assert.True(t, bump.Before(run), "%s: bump %s is not before run %s", id, bump, run)
		assert.LessOrEqual(t, run.Sub(bump), schedule.BumpLead, "%s: lead is longer than BumpLead", id)
		assert.LessOrEqual(t, run.Sub(bump), offset, "%s: bump precedes the window opening", id)
	}
}

func TestNextBump_RejectsMalformedSpecs(t *testing.T) {
	spec := window("Europe/Zurizh", "sunday")

	_, err := schedule.NextBump(spec, "db-prod", time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Europe/Zurizh")
}

func TestPrevRun(t *testing.T) {
	loc := zurich(t)
	spec := window("Europe/Zurich", "sunday") // db-prod starts 23:19

	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "earlier the same day takes last week",
			now:  time.Date(2026, 9, 13, 12, 0, 0, 0, loc),
			want: time.Date(2026, 9, 6, 23, 19, 0, 0, loc),
		},
		{
			name: "standing on the start counts as that start",
			now:  time.Date(2026, 9, 13, 23, 19, 0, 0, loc),
			want: time.Date(2026, 9, 13, 23, 19, 0, 0, loc),
		},
		{
			name: "after the start takes it",
			now:  time.Date(2026, 9, 14, 1, 0, 0, 0, loc),
			want: time.Date(2026, 9, 13, 23, 19, 0, 0, loc),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := schedule.PrevRun(spec, "db-prod", tt.now)
			require.NoError(t, err)
			assertSameInstant(t, tt.want, got)
		})
	}
}

func TestPrevRun_IsTheOccurrenceBeforeNextRun(t *testing.T) {
	loc := zurich(t)
	spec := window("Europe/Zurich", "sunday", "wednesday")

	for h := range 24 {
		now := time.Date(2026, 9, 16, h, 30, 0, 0, loc)

		prev, err := schedule.PrevRun(spec, "db-prod", now)
		require.NoError(t, err)
		next, err := schedule.NextRun(spec, "db-prod", now)
		require.NoError(t, err)

		assert.False(t, prev.After(now), "%s: prev %s is in the future", now, prev)
		assert.True(t, next.After(now), "%s: next %s is not in the future", now, next)
		// Nothing may sit between them: they are adjacent occurrences.
		mid, err := schedule.NextRun(spec, "db-prod", prev)
		require.NoError(t, err)
		assertSameInstant(t, next, mid)
	}
}

func TestBumpDue(t *testing.T) {
	loc := zurich(t)
	spec := window("Europe/Zurich", "sunday") // db-prod: bump 23:04, run 23:19

	thisWeek := time.Date(2026, 9, 13, 23, 4, 0, 0, loc)
	lastWeek := time.Date(2026, 9, 6, 23, 4, 0, 0, loc)

	tests := []struct {
		name           string
		now            time.Time
		wantOccurrence time.Time
		wantOpen       bool
	}{
		{
			name:           "at the lead",
			now:            time.Date(2026, 9, 13, 23, 4, 0, 0, loc),
			wantOccurrence: thisWeek,
			wantOpen:       true,
		},
		{
			name:           "inside the lead",
			now:            time.Date(2026, 9, 13, 23, 10, 0, 0, loc),
			wantOccurrence: thisWeek,
			wantOpen:       true,
		},
		{
			name: "after the maintenance started but still in the window",
			// A restart that missed the lead catches up here rather than
			// waiting a week.
			now:            time.Date(2026, 9, 14, 1, 0, 0, 0, loc),
			wantOccurrence: thisWeek,
			wantOpen:       true,
		},
		{
			name:           "the window has closed",
			now:            time.Date(2026, 9, 14, 4, 0, 0, 0, loc),
			wantOccurrence: thisWeek,
			wantOpen:       false,
		},
		{
			name:           "before this week's lead is still last week's occurrence",
			now:            time.Date(2026, 9, 13, 23, 3, 0, 0, loc),
			wantOccurrence: lastWeek,
			wantOpen:       false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			occurrence, open, err := schedule.BumpDue(spec, "db-prod", tt.now)
			require.NoError(t, err)
			assertSameInstant(t, tt.wantOccurrence, occurrence)
			assert.Equal(t, tt.wantOpen, open)
		})
	}
}

func TestBumpDue_AZeroOffsetInstanceIsStillDue(t *testing.T) {
	// The hole a "is it the lead right now" check falls into: svc/instance-79
	// has a zero offset, so its lead is zero-length and no instant lies inside
	// it. Keyed on the occurrence instead, it is due like any other.
	loc := zurich(t)
	spec := window("Europe/Zurich", "sunday")
	require.Zero(t, schedule.Offset(spec, "svc/instance-79"), "identity picked for its zero offset")

	occurrence, open, err := schedule.BumpDue(spec, "svc/instance-79",
		time.Date(2026, 9, 13, 22, 0, 0, 0, loc))
	require.NoError(t, err)
	assert.True(t, open)
	assertSameInstant(t, time.Date(2026, 9, 13, 22, 0, 0, 0, loc), occurrence)
}

func TestBumpDue_OccurrenceIsStableAcrossTheWindow(t *testing.T) {
	// Every reconcile inside one window must report the same occurrence, or a
	// caller comparing it against what it recorded would act repeatedly.
	loc := zurich(t)
	spec := window("Europe/Zurich", "sunday")

	want, _, err := schedule.BumpDue(spec, "db-prod", time.Date(2026, 9, 13, 23, 4, 0, 0, loc))
	require.NoError(t, err)

	for _, now := range []time.Time{
		time.Date(2026, 9, 13, 23, 5, 0, 0, loc),
		time.Date(2026, 9, 13, 23, 19, 0, 0, loc),
		time.Date(2026, 9, 14, 2, 30, 0, 0, loc),
		time.Date(2026, 9, 14, 3, 59, 0, 0, loc),
	} {
		got, open, err := schedule.BumpDue(spec, "db-prod", now)
		require.NoError(t, err)
		assert.True(t, open, "%s is inside the window", now)
		assertSameInstant(t, want, got)
	}
}
