package slaproxy

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type minuteInterval struct{ start, end int }
type weeklyWindows [7][]minuteInterval

// compileSchedule splits overnight windows onto the actual weekdays they cover.
// This detects overlap between adjacent weekdays and across the week wrap.
func compileSchedule(schedule map[string][]string) (weeklyWindows, error) {
	var week weeklyWindows
	days := map[string]time.Weekday{
		"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday,
		"wednesday": time.Wednesday, "thursday": time.Thursday,
		"friday": time.Friday, "saturday": time.Saturday,
	}
	for name, windows := range schedule {
		day, ok := days[name]
		if !ok {
			return week, fmt.Errorf("invalid weekday %q", name)
		}
		for _, window := range windows {
			parts := strings.Split(window, "-")
			if len(parts) != 2 {
				return week, fmt.Errorf("invalid time window %q", window)
			}
			start, e1 := clockMinutes(parts[0], false)
			end, e2 := clockMinutes(parts[1], true)
			if e1 != nil || e2 != nil || start == end {
				return week, fmt.Errorf("invalid time window %q", window)
			}
			if end > start {
				week[day] = append(week[day], minuteInterval{start, end})
			} else {
				week[day] = append(week[day], minuteInterval{start, 1440})
				if end > 0 {
					next := (int(day) + 1) % 7
					week[next] = append(week[next], minuteInterval{0, end})
				}
			}
		}
	}
	for day := range week {
		windows := week[day]
		sort.Slice(windows, func(i, j int) bool { return windows[i].start < windows[j].start })
		for i := 1; i < len(windows); i++ {
			if windows[i].start < windows[i-1].end {
				return week, fmt.Errorf("overlapping windows on %s, including overnight carry", time.Weekday(day))
			}
		}
	}
	return week, nil
}
