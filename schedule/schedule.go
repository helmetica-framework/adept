// Package schedule renders a MaintenanceWindow as a Kubernetes CronJob
// schedule.
//
// A window declares when maintenance may start: a wall-clock time on a set of
// weekdays, in an IANA time zone, staying open for a duration. Instances
// sharing a window are spread across that span by an offset derived from the
// instance, so a window governing many instances does not start all of them at
// once.
//
// Kubernetes owns the clock for anything a CronJob fires: CronSchedule
// produces only the schedule string and time zone to put on one, and the
// CronJob controller decides when it runs, including across daylight-saving
// changes. NextRun is for the callers that have no CronJob to do that for them
// and must wake at the same moment themselves.
//
// Nothing here reads the cluster. Every function is pure and takes the spec by
// value, so the whole package is testable without an API server.
package schedule

import (
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
	"time"

	// The IANA time zone database, embedded in the binary.
	//
	// Do not remove this. The manager image is
	// gcr.io/distroless/static:nonroot, which ships no /usr/share/zoneinfo, so
	// without it every window with a non-UTC time zone resolves fine on a
	// developer machine and fails in the cluster.
	_ "time/tzdata"

	"github.com/robfig/cron/v3"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
)

const (
	clockLayout = "15:04"

	minDuration = time.Hour
	maxDuration = 24 * time.Hour

	minutesPerDay = 24 * 60

	// BumpLead is how far ahead of an instance's maintenance start work that has
	// to happen first is scheduled.
	BumpLead = 15 * time.Minute
)

// Weekdays provided by the time package, already map cleanly to cron.
// Sunday = 0, Saturday = 6
var weekdays = map[ritualsv1.Day]time.Weekday{
	ritualsv1.Sunday:    time.Sunday,
	ritualsv1.Monday:    time.Monday,
	ritualsv1.Tuesday:   time.Tuesday,
	ritualsv1.Wednesday: time.Wednesday,
	ritualsv1.Thursday:  time.Thursday,
	ritualsv1.Friday:    time.Friday,
	ritualsv1.Saturday:  time.Saturday,
}

// Validate reports whether spec is resolvable: days that are known and not
// repeated, a parseable HH:MM time, a duration between 1h and 24h, and a
// loadable IANA time zone. An empty time zone means UTC.
//
// This is the single definition of what a valid window is. The CRD schema
// declares the type's shape and little else, so the API server will happily
// store a window this rejects. Check before relying on one.
func Validate(spec ritualsv1.MaintenanceWindowSpec) error {
	if _, err := time.LoadLocation(spec.TimeZone); err != nil {
		return fmt.Errorf("time zone %q: %w", spec.TimeZone, err)
	}

	if len(spec.DaysOfWeek) == 0 {
		return fmt.Errorf("daysOfWeek must name at least one day")
	}

	seen := make(map[ritualsv1.Day]bool, len(spec.DaysOfWeek))
	for _, day := range spec.DaysOfWeek {
		if _, ok := weekdays[day]; !ok {
			return fmt.Errorf("unknown day %q", day)
		}
		if seen[day] {
			return fmt.Errorf("day %q is listed more than once", day)
		}
		seen[day] = true
	}

	if _, err := time.Parse(clockLayout, spec.Time); err != nil {
		return fmt.Errorf("time %q must be HH:MM in 24-hour form: %w", spec.Time, err)
	}

	if d := spec.Duration.Duration; d < minDuration || d > maxDuration {
		return fmt.Errorf("duration %s must be between %s and %s", d, minDuration, maxDuration)
	}

	return nil
}

// Offset returns the deterministic delay applied to identity's window start.
// The result is a whole number of minutes in [0, Duration), spreading
// instances across the whole span in which maintenance may start.
// identity is usually the instance namespace.
//
// It must stay stable across process restarts and adept upgrades: changing how
// it is derived moves every instance in every cluster to a different minute.
// The tests pin it with golden values for that reason.
func Offset(spec ritualsv1.MaintenanceWindowSpec, identity string) time.Duration {
	oh := fnv.New64a()
	oh.Write([]byte(identity))
	sum := oh.Sum64()

	span := int64(spec.Duration.Duration / time.Minute)
	if span <= 0 {
		return 0
	}

	return time.Duration(sum%uint64(span)) * time.Minute
}

// CronSchedule renders identity's window start as a Kubernetes CronJob
// schedule and time zone, ready for CronJob.spec.schedule and
// CronJob.spec.timeZone.
func CronSchedule(spec ritualsv1.MaintenanceWindowSpec, identity string) (schedule, timeZone string, err error) {
	if err := Validate(spec); err != nil {
		return "", "", err
	}

	opens, err := time.Parse(clockLayout, spec.Time)
	if err != nil {
		return "", "", fmt.Errorf("time %q: %w", spec.Time, err)
	}

	// Minutes past midnight, plus this instance's share of the spread. The sum
	// can run past midnight, which moves the start onto the following day.
	total := opens.Hour()*60 + opens.Minute() + int(Offset(spec, identity)/time.Minute)
	dayShift := total / minutesPerDay
	hour, minute := total%minutesPerDay/60, total%60

	// Every listed day shifts by the same amount, because the offset is per
	// instance rather than per day. Missing this is how a "sunday 23:00"
	// window ends up running every affected instance a full day early.
	days := make([]int, 0, len(spec.DaysOfWeek))
	for _, day := range spec.DaysOfWeek {
		days = append(days, (int(weekdays[day])+dayShift)%7)
	}
	// Sorted so the schedule is stable. An order that followed the spec would
	// rewrite the CronJob whenever someone reordered daysOfWeek.
	slices.Sort(days)

	fields := make([]string, len(days))
	for i, day := range days {
		fields[i] = strconv.Itoa(day)
	}

	timeZone = spec.TimeZone
	if timeZone == "" {
		timeZone = "UTC"
	}

	return fmt.Sprintf("%d %d * * %s", minute, hour, strings.Join(fields, ",")), timeZone, nil
}

// NextRun returns the first moment strictly after now at which identity's
// maintenance starts, in the window's time zone. It is the same instant the
// CronJob built from CronSchedule fires, for callers that have to act on the
// schedule without one.
//
// Strictly after, so that a caller waking at its own start time computes the
// following occurrence rather than the one it just handled.
func NextRun(spec ritualsv1.MaintenanceWindowSpec, identity string, now time.Time) (time.Time, error) {
	parsed, err := parse(spec, identity)
	if err != nil {
		return time.Time{}, err
	}

	return parsed.Next(now), nil
}

// parse renders identity's schedule and hands it to the same cron parser
// Kubernetes runs the CronJob on.
func parse(spec ritualsv1.MaintenanceWindowSpec, identity string) (cron.Schedule, error) {
	cronSpec, tz, err := CronSchedule(spec, identity)
	if err != nil {
		return nil, fmt.Errorf("generating cron schedule: %w", err)
	}

	// CRON_TZ is not decoration: an unprefixed expression is parsed as
	// time.Local, which is the machine's zone rather than the window's.
	parsed, err := cron.ParseStandard(fmt.Sprintf("CRON_TZ=%s %s", tz, cronSpec))
	if err != nil {
		return nil, fmt.Errorf("parsing schedule %q: %w", cronSpec, err)
	}

	return parsed, nil
}

// NextBump returns the first moment strictly after now at which a maintenance should run:
// BumpLead ahead of the start, or the instance's whole offset when that is shorter,
// so a bump never lands before the window has opened.
//
// A now that already sits between the bump and the start takes the following
// occurrence. Missing a bump leaves the instance as it is for another window,
// which is the safe direction: it never acts outside one.
func NextBump(spec ritualsv1.MaintenanceWindowSpec, identity string, now time.Time) (time.Time, error) {
	lead := min(BumpLead, Offset(spec, identity))

	run, err := NextRun(spec, identity, now)
	if err != nil {
		return time.Time{}, err
	}

	if bump := run.Add(-lead); bump.After(now) {
		return bump, nil
	}

	// now is already inside the lead, so this window's bump has gone.
	run, err = NextRun(spec, identity, run)
	if err != nil {
		return time.Time{}, err
	}

	return run.Add(-lead), nil
}

// PrevRun returns the most recent moment at or before now at which identity's
// maintenance started. A window names at least one weekday, so an occurrence
// always exists within the week before now.
func PrevRun(spec ritualsv1.MaintenanceWindowSpec, identity string, now time.Time) (time.Time, error) {
	parsed, err := parse(spec, identity)
	if err != nil {
		return time.Time{}, err
	}

	// Walked forward from eight days back, because the parser offers Next and
	// nothing else. Eight days always contains an occurrence of any weekday.
	var prev time.Time
	for cursor := now.Add(-8 * 24 * time.Hour); ; {
		next := parsed.Next(cursor)
		if next.After(now) {
			break
		}
		prev, cursor = next, next
	}

	if prev.IsZero() {
		return time.Time{}, fmt.Errorf("no maintenance in the eight days before %s", now)
	}

	return prev, nil
}

// BumpDue reports the maintenance whose lead now falls in or after, and
// whether that maintenance's window is still open.
//
// The occurrence is what a caller records once it has acted, so that a
// restart, a re-list or an unrelated event does not act on it twice. Comparing
// against a recorded occurrence is what makes this safe where a plain "is it
// the lead right now" check is not: an instance whose offset is zero has a
// zero-length lead and would never match one.
func BumpDue(spec ritualsv1.MaintenanceWindowSpec, identity string, now time.Time) (occurrence time.Time, open bool, err error) {
	next, err := NextRun(spec, identity, now)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("getting next run: %w", err)
	}

	offset := Offset(spec, identity)
	lead := min(BumpLead, offset)

	occ := next.Add(-lead)

	// At or after its lead, the coming maintenance is the one to account for.
	// Standing exactly on it counts, or the instant a caller wakes for would
	// send it back to the previous one.
	if occ.After(now) {
		prev, err := PrevRun(spec, identity, now)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("getting previous run: %w", err)
		}
		occ = prev.Add(-lead)
	}

	// The window opened one offset before the maintenance and stays open for
	// its whole duration, whatever the instance's share of it.
	closes := occ.Add(lead - offset + spec.Duration.Duration)

	return occ, now.Before(closes), nil
}
