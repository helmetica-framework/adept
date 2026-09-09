// Package schedule renders a MaintenanceWindow as a Kubernetes CronJob
// schedule.
//
// A window declares when maintenance may start: a wall-clock time on a set of
// weekdays, in an IANA time zone, staying open for a duration. Instances
// sharing a window are spread across that span by an offset derived from the
// instance, so a window governing many instances does not start all of them at
// once.
//
// Kubernetes owns the clock. This package produces only the schedule string
// and time zone to put on a CronJob; the CronJob controller decides when to
// fire, including across daylight-saving changes. Nothing here computes or
// tracks individual occurrences, because a caller holding a CronJob has no use
// for them.
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

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
)

const (
	clockLayout = "15:04"

	minDuration = time.Hour
	maxDuration = 24 * time.Hour

	minutesPerDay = 24 * 60
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
