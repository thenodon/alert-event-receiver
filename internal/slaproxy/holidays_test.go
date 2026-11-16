package slaproxy

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHolidayCoverageAndMovableDates(t *testing.T) {
	provider := CountryHolidays{}
	for _, tc := range []struct {
		country, subdivision, date string
		holiday                    bool
	}{
		{"SE", "", "2026-04-03", true}, {"SE", "", "2027-03-26", true}, {"SE", "", "2028-04-14", true},
		{"SE", "", "2026-04-06", true}, {"SE", "", "2026-05-14", true}, {"SE", "", "2026-06-20", true},
		{"SE", "", "2026-06-19", false}, {"SE", "", "2026-12-24", false}, {"SE", "", "2026-12-25", true},
		{"SE", "", "2026-12-31", false}, {"SE", "", "2027-01-01", true}, {"SE", "", "2026-11-11", false},
		{"DE", "", "2026-04-03", true}, {"DE", "", "2027-03-29", true}, {"DE", "", "2026-10-03", true},
		{"DE", "", "2026-01-06", false}, {"DE", "BY", "2026-01-06", true},
		{"DE", "", "2026-06-04", false}, {"DE", "BY", "2026-06-04", true},
		{"DE", "BY", "2027-05-27", true}, {"DE", "BY", "2026-11-01", true},
		// These depend on municipality and are not in the statewide list.
		{"DE", "BY", "2026-08-08", false}, {"DE", "BY", "2026-08-15", false},
	} {
		t.Run(tc.country+tc.subdivision+tc.date, func(t *testing.T) {
			if err := provider.Validate(tc.country, tc.subdivision); err != nil {
				t.Fatal(err)
			}
			date := instant(t, tc.date+"T00:00:00+14:00") // Preserve civil date, not UTC date.
			got, err := provider.IsHoliday(context.Background(), date, tc.country, tc.subdivision)
			if err != nil || got != tc.holiday {
				t.Fatalf("holiday=%v, want %v: %v", got, tc.holiday, err)
			}
		})
	}
	for _, region := range [][2]string{{"SE", "BY"}, {"DE", "BW"}, {"ZZ", ""}, {"", ""}} {
		if err := provider.Validate(region[0], region[1]); err == nil {
			t.Fatal("unsupported region accepted", region)
		}
		if _, err := provider.IsHoliday(context.Background(), time.Now(), region[0], region[1]); err == nil {
			t.Fatal("unsupported region silently evaluated", region)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.IsHoliday(ctx, time.Now(), "SE", ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestUnsupportedHolidayCoverageFailsServiceStartup(t *testing.T) {
	for _, region := range [][2]string{{"ZZ", ""}, {"SE", "UNVERIFIED"}, {"DE", "BW"}} {
		cfg := testConfig(t)
		policy := cfg.SLADefinitions[0].SLAs["business"]
		policy.Holidays = &Holidays{Country: region[0], Subdivision: region[1]}
		cfg.SLADefinitions[0].SLAs["business"] = policy
		if _, err := NewCalendarEngine(cfg, CountryHolidays{}); err == nil {
			t.Fatal("unsupported coverage accepted at startup", region)
		}
	}
}
