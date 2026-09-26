package launchd

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// MaxIntervals caps how many StartCalendarInterval entries a schedule may
// expand to, so that a schedule such as "*/5 9-17 * * 1-5" (540 entries)
// cannot produce a huge plist.
const MaxIntervals = 500

// CalendarInterval is one StartCalendarInterval entry. A nil field means
// "every" (launchd treats a missing key as a wildcard). Weekday 0 is Sunday.
type CalendarInterval struct {
	Minute  *int
	Hour    *int
	Day     *int
	Month   *int
	Weekday *int
}

// cronField describes one of the five cron fields.
type cronField struct {
	name     string
	min, max int
	// sunday7 marks day-of-week, where 7 is another name for 0 (Sunday).
	sunday7 bool
}

// Field indexes into cronFields, in cron order.
const (
	fMinute = iota
	fHour
	fDay
	fMonth
	fWeekday
)

var cronFields = [5]cronField{
	fMinute:  {name: "minute", min: 0, max: 59},
	fHour:    {name: "hour", min: 0, max: 23},
	fDay:     {name: "day-of-month", min: 1, max: 31},
	fMonth:   {name: "month", min: 1, max: 12},
	fWeekday: {name: "day-of-week", min: 0, max: 7, sunday7: true},
}

// ParseSchedule converts a 5-field cron expression (minute hour day-of-month
// month day-of-week, local time) into the minimal set of launchd calendar
// intervals. For example "0 9 * * 1-5" yields five entries, one per weekday.
//
// Each field accepts a number, "*", a range "a-b", a step "*/n" or "a-b/n", or
// a comma-separated list of those. Day-of-week 0 and 7 both mean Sunday. Names
// such as MON or JAN, macros such as @daily, and schedules that expand to more
// than MaxIntervals entries are errors.
//
// Setting both day-of-month and day-of-week to anything but "*" is also an
// error. Cron then fires when either one matches (OR), but launchd's semantics
// for an interval with both Day and Weekday differ, and a cartesian product
// would mean AND, so "0 9 1 * 1" would silently run on other days than cron
// would. Use "*" for one of the two fields, or split it into two routines.
func ParseSchedule(cron string) ([]CalendarInterval, error) {
	if strings.HasPrefix(strings.TrimSpace(cron), "@") {
		return nil, fmt.Errorf("schedule %q: macros such as @daily are not supported; use 5 cron fields", cron)
	}
	parts := strings.Fields(cron)
	if len(parts) != len(cronFields) {
		return nil, fmt.Errorf("schedule %q: want 5 fields (minute hour day-of-month month day-of-week), got %d", cron, len(parts))
	}

	// sets[i] is nil when field i matches every value.
	var sets [5][]int
	total := 1
	for i, f := range cronFields {
		vals, err := f.parse(parts[i])
		if err != nil {
			return nil, fmt.Errorf("schedule %q: %w", cron, err)
		}
		sets[i] = vals
		total *= max(len(vals), 1)
	}
	if parts[fDay] != "*" && parts[fWeekday] != "*" {
		return nil, fmt.Errorf("schedule %q: day-of-month and day-of-week cannot both be set (cron matches either one, launchd differs); use * for one of them", cron)
	}
	if total > MaxIntervals {
		return nil, fmt.Errorf("schedule %q: expands to %d calendar intervals, more than %d", cron, total, MaxIntervals)
	}

	// Take the cartesian product in chronological order: month, day,
	// weekday, hour, minute.
	combos := [][5]int{{}}
	for _, i := range []int{fMonth, fDay, fWeekday, fHour, fMinute} {
		if sets[i] == nil {
			continue
		}
		next := make([][5]int, 0, len(combos)*len(sets[i]))
		for _, c := range combos {
			for _, v := range sets[i] {
				c[i] = v
				next = append(next, c)
			}
		}
		combos = next
	}

	out := make([]CalendarInterval, len(combos))
	for n, c := range combos {
		pick := func(i int) *int {
			if sets[i] == nil {
				return nil
			}
			v := c[i]
			return &v
		}
		out[n] = CalendarInterval{
			Minute:  pick(fMinute),
			Hour:    pick(fHour),
			Day:     pick(fDay),
			Month:   pick(fMonth),
			Weekday: pick(fWeekday),
		}
	}
	return out, nil
}

// parse returns the sorted, de-duplicated values a field matches, or nil when
// it matches every value of the field.
func (f cronField) parse(s string) ([]int, error) {
	seen := make(map[int]bool)
	for item := range strings.SplitSeq(s, ",") {
		lo, hi, step, err := f.parseItem(item)
		if err != nil {
			return nil, fmt.Errorf("%s field %q: %w", f.name, s, err)
		}
		for v := lo; v <= hi; v += step {
			if f.sunday7 && v == 7 {
				seen[0] = true
			} else {
				seen[v] = true
			}
		}
	}
	all := f.max - f.min + 1
	if f.sunday7 {
		all-- // 0 and 7 are the same day.
	}
	if len(seen) == all {
		return nil, nil
	}
	vals := make([]int, 0, len(seen))
	for v := range seen {
		vals = append(vals, v)
	}
	slices.Sort(vals)
	return vals, nil
}

// parseItem parses one list item into an inclusive range and a step.
func (f cronField) parseItem(item string) (lo, hi, step int, err error) {
	if item == "" {
		return 0, 0, 0, errors.New("empty list item")
	}
	rng, stepStr, hasStep := strings.Cut(item, "/")
	step = 1
	if hasStep {
		if step, err = number(stepStr); err != nil {
			return 0, 0, 0, fmt.Errorf("step: %w", err)
		}
		if step < 1 {
			return 0, 0, 0, errors.New("step must be at least 1")
		}
	}

	if rng == "*" {
		return f.min, f.max, step, nil
	}
	loStr, hiStr, isRange := strings.Cut(rng, "-")
	if !isRange && hasStep {
		return 0, 0, 0, fmt.Errorf("%q: a step needs * or a range, as in */n or a-b/n", item)
	}
	if lo, err = f.value(loStr); err != nil {
		return 0, 0, 0, err
	}
	hi = lo
	if isRange {
		if hi, err = f.value(hiStr); err != nil {
			return 0, 0, 0, err
		}
		if lo > hi {
			return 0, 0, 0, fmt.Errorf("range %q runs backwards", rng)
		}
	}
	return lo, hi, step, nil
}

// value parses a single field value and checks its bounds.
func (f cronField) value(s string) (int, error) {
	v, err := number(s)
	if err != nil {
		return 0, err
	}
	if v < f.min || v > f.max {
		return 0, fmt.Errorf("%d is outside %d-%d", v, f.min, f.max)
	}
	return v, nil
}

// number parses a non-negative decimal number. Names such as MON are rejected.
func number(s string) (int, error) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, fmt.Errorf("%q is not a number (names such as MON or JAN are not supported)", s)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", s, err)
	}
	return n, nil
}
