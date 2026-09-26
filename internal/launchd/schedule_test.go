package launchd

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// describe renders intervals compactly, e.g. "Hour=9 Minute=0 Weekday=1".
func describe(cis []CalendarInterval) []string {
	out := make([]string, len(cis))
	for i, ci := range cis {
		var parts []string
		for _, f := range ci.fields() {
			parts = append(parts, fmt.Sprintf("%s=%d", f.key, *f.val))
		}
		out[i] = strings.Join(parts, " ")
	}
	return out
}

func TestParseSchedule(t *testing.T) {
	tests := []struct {
		cron string
		want []string
	}{
		{"0 9 * * 1-5", []string{
			"Hour=9 Minute=0 Weekday=1",
			"Hour=9 Minute=0 Weekday=2",
			"Hour=9 Minute=0 Weekday=3",
			"Hour=9 Minute=0 Weekday=4",
			"Hour=9 Minute=0 Weekday=5",
		}},
		{"30 8 * * *", []string{"Hour=8 Minute=30"}},
		{"  30\t8 *  * * ", []string{"Hour=8 Minute=30"}},
		{"00 09 * * *", []string{"Hour=9 Minute=0"}},
		{"* * * * *", []string{""}},
		{"*/1 * * * *", []string{""}},
		{"0-59 0-23 1-31 1-12 *", []string{""}},
		{"*/15 * * * *", []string{"Minute=0", "Minute=15", "Minute=30", "Minute=45"}},
		{"0 */6 * * *", []string{"Hour=0 Minute=0", "Hour=6 Minute=0", "Hour=12 Minute=0", "Hour=18 Minute=0"}},
		{"0 10-16/3 * * *", []string{"Hour=10 Minute=0", "Hour=13 Minute=0", "Hour=16 Minute=0"}},
		{"0 */100 * * *", []string{"Hour=0 Minute=0"}},
		// An unset minute means every minute of the listed hours.
		{"* 9-10 * * *", []string{"Hour=9", "Hour=10"}},
		{"5,5,5 * * * *", []string{"Minute=5"}},
		{"45,15 * * * *", []string{"Minute=15", "Minute=45"}},
		{"0 9 1,15 * *", []string{"Day=1 Hour=9 Minute=0", "Day=15 Hour=9 Minute=0"}},
		{"0 9 1 1-3 *", []string{
			"Day=1 Hour=9 Minute=0 Month=1",
			"Day=1 Hour=9 Minute=0 Month=2",
			"Day=1 Hour=9 Minute=0 Month=3",
		}},
		{"0 9 * * 1,3-5", []string{
			"Hour=9 Minute=0 Weekday=1",
			"Hour=9 Minute=0 Weekday=3",
			"Hour=9 Minute=0 Weekday=4",
			"Hour=9 Minute=0 Weekday=5",
		}},
		// 0 and 7 are both Sunday.
		{"0 9 * * 7", []string{"Hour=9 Minute=0 Weekday=0"}},
		{"0 9 * * 0,7", []string{"Hour=9 Minute=0 Weekday=0"}},
		{"0 9 * * 5-7", []string{"Hour=9 Minute=0 Weekday=0", "Hour=9 Minute=0 Weekday=5", "Hour=9 Minute=0 Weekday=6"}},
		{"0 9 * * 1-7", []string{"Hour=9 Minute=0"}},
		{"0 9 * * 0-6", []string{"Hour=9 Minute=0"}},
		// Chronological order: weekday, then hour, then minute.
		{"0,30 9,17 * * 1", []string{
			"Hour=9 Minute=0 Weekday=1",
			"Hour=9 Minute=30 Weekday=1",
			"Hour=17 Minute=0 Weekday=1",
			"Hour=17 Minute=30 Weekday=1",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.cron, func(t *testing.T) {
			got, err := ParseSchedule(tt.cron)
			if err != nil {
				t.Fatalf("ParseSchedule(%q): %v", tt.cron, err)
			}
			if d := describe(got); !slices.Equal(d, tt.want) {
				t.Errorf("ParseSchedule(%q) =\n  %q\nwant\n  %q", tt.cron, d, tt.want)
			}
		})
	}
}

func TestParseScheduleIntervalsDoNotShareValues(t *testing.T) {
	got, err := ParseSchedule("0 9 * * 1-5")
	if err != nil {
		t.Fatal(err)
	}
	*got[0].Hour = 10
	if *got[1].Hour != 9 {
		t.Errorf("changing one interval changed another: Hour = %d", *got[1].Hour)
	}
}

func TestParseScheduleLimit(t *testing.T) {
	// 10 * 10 * 5 = 500 is the maximum.
	got, err := ParseSchedule("0-9 0-9 1-5 * *")
	if err != nil {
		t.Fatalf("500 intervals: %v", err)
	}
	if len(got) != MaxIntervals {
		t.Errorf("got %d intervals, want %d", len(got), MaxIntervals)
	}
}

func TestParseScheduleErrors(t *testing.T) {
	tests := []struct {
		cron, want string
	}{
		{"", "want 5 fields"},
		{"0 9 * *", "want 5 fields"},
		{"0 9 * * * *", "want 5 fields"},
		{"@daily", "macros"},
		{" @reboot", "macros"},
		{"0 9 * * MON", "names such as MON"},
		{"0 9 * JAN *", "names such as MON"},
		{"0 9 * * mon-fri", "not a number"},
		{"60 * * * *", "60 is outside 0-59"},
		{"* 24 * * *", "24 is outside 0-23"},
		{"* * 0 * *", "0 is outside 1-31"},
		{"* * 32 * *", "32 is outside 1-31"},
		{"* * * 0 *", "0 is outside 1-12"},
		{"* * * 13 *", "13 is outside 1-12"},
		{"* * * * 8", "8 is outside 0-7"},
		{"5-1 * * * *", "runs backwards"},
		{"*/0 * * * *", "at least 1"},
		{"*/ * * * *", "not a number"},
		{"*/x * * * *", "not a number"},
		{"5/10 * * * *", "a step needs"},
		{"1-2-3 * * * *", "not a number"},
		{"1,,2 * * * *", "empty list item"},
		{"1, * * * *", "empty list item"},
		{"-1 * * * *", "not a number"},
		{"+1 * * * *", "not a number"},
		{"? * * * *", "not a number"},
		{"0 9 L * *", "not a number"},
		{"0 9 1 * 1", "day-of-month and day-of-week"},
		{"0 9 1-31 * 1", "day-of-month and day-of-week"},
		{"0 9 */2 * 1-5", "day-of-month and day-of-week"},
		{"*/5 9-17 * * 1-5", "540 calendar intervals"},
		{"0-9 0-9 1-6 * *", "600 calendar intervals"},
	}
	for _, tt := range tests {
		t.Run(tt.cron, func(t *testing.T) {
			got, err := ParseSchedule(tt.cron)
			if err == nil {
				t.Fatalf("ParseSchedule(%q) = %q, want error", tt.cron, describe(got))
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseSchedule(%q) error = %q, want it to contain %q", tt.cron, err, tt.want)
			}
		})
	}
}
