// Package schedule dispatches approved jobs from five-field cron schedules.
package schedule

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

type rule struct {
	cron     cron.Schedule
	location *time.Location
}

func parseRule(expression, timezone string) (rule, error) {
	if len(strings.Fields(expression)) != 5 || len(expression) > 256 || strings.Contains(expression, "=") {
		return rule{}, fmt.Errorf("cron requires five fields (minute hour day-of-month month day-of-week)")
	}
	if timezone == "" || timezone == "Local" {
		return rule{}, fmt.Errorf("specify an IANA time zone such as America/Winnipeg or UTC")
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return rule{}, fmt.Errorf("unknown IANA time zone")
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	parsed, err := parser.Parse(expression)
	if err != nil {
		return rule{}, fmt.Errorf("invalid cron expression: %w", err)
	}
	return rule{parsed, location}, nil
}

// Evaluate each constant-offset UTC interval separately. The cron library sees a
// UTC calendar representing local wall time; mapping the result back into its
// interval removes spring gaps and makes offsets of 30 or 45 minutes work too.
func (r rule) next(after time.Time) time.Time {
	cursor := after.UTC().Truncate(time.Minute).Add(time.Minute)
	for {
		local := cursor.In(r.location)
		start, end := local.ZoneBounds()
		_, offset := local.Zone()
		if !start.IsZero() {
			_, prior := start.Add(-time.Second).Zone()
			firstNewSlot := start.Add(time.Duration(prior-offset) * time.Second)
			if prior > offset && cursor.Before(firstNewSlot) {
				// A backward transition repeats slots already represented by the prior interval.
				cursor = firstNewSlot.UTC()
				continue
			}
		}
		shift := time.Duration(offset) * time.Second
		wall := cursor.Add(shift).Add(-time.Second)
		nextWall := r.cron.Next(wall)
		if nextWall.IsZero() {
			// Cron searches five years. Leap days can be eight years apart across a century.
			nextWall = r.cron.Next(wall.AddDate(5, 0, 0))
			if nextWall.IsZero() {
				return time.Time{}
			}
		}
		candidate := nextWall.Add(-shift)
		if end.IsZero() || candidate.Before(end) {
			return candidate.UTC()
		}
		// Some zoneinfo extrapolations return the end of the preceding interval
		// during the final day of a leap year. Advance at least a minute in that case.
		if !end.After(cursor) {
			cursor = cursor.Add(time.Minute)
		} else {
			cursor = end.UTC()
		}
	}
}

// NextOccurrence shares timezone and repeated-DST-slot handling with personal runtimes.
func NextOccurrence(expression, timezone string, after time.Time) (time.Time, error) {
	r, err := parseRule(expression, timezone)
	if err != nil {
		return time.Time{}, err
	}
	return r.next(after), nil
}
