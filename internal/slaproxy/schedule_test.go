package slaproxy

import "testing"

func TestCrossWeekdayAndWeekWrapOverlapValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		schedule map[string][]string
		valid    bool
	}{
		{"overnight collision", map[string][]string{"monday": {"22:00-06:00"}, "tuesday": {"05:00-08:00"}}, false},
		{"overnight adjacent", map[string][]string{"monday": {"22:00-06:00"}, "tuesday": {"06:00-08:00"}}, true},
		{"Sunday Monday collision", map[string][]string{"sunday": {"22:00-06:00"}, "monday": {"05:00-08:00"}}, false},
		{"Saturday Sunday collision", map[string][]string{"saturday": {"22:00-06:00"}, "sunday": {"05:00-08:00"}}, false},
		{"end at midnight", map[string][]string{"monday": {"22:00-00:00"}, "tuesday": {"00:00-06:00"}}, true},
		{"full next day collision", map[string][]string{"monday": {"22:00-06:00"}, "tuesday": {"00:00-24:00"}}, false},
		{"full adjacent days", map[string][]string{"monday": {"00:00-24:00"}, "tuesday": {"00:00-24:00"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := compileSchedule(tc.schedule); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}
