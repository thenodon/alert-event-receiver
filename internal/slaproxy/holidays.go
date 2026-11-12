package slaproxy

import (
	"context"
	"fmt"
	"time"

	"github.com/rickar/cal/v2"
	"github.com/rickar/cal/v2/de"
	"github.com/rickar/cal/v2/se"
)

const holidayRulesVersion = "rickar/cal/v2@v2.1.32:public-v1"

// CountryHolidays is an offline adapter with explicit, tested coverage. Public
// holidays only: Swedish holiday eves and municipality-specific Bavarian dates
// require explicit additional_dates. No library global state is modified.
type CountryHolidays struct{}

func (CountryHolidays) Version() string { return holidayRulesVersion }

func (CountryHolidays) Validate(country, subdivision string) error {
	_, err := countryHolidays(country, subdivision)
	return err
}

func countryHolidays(country, subdivision string) ([]*cal.Holiday, error) {
	switch {
	case country == "SE" && subdivision == "":
		return se.Holidays, nil
	case country == "DE" && subdivision == "":
		return de.Holidays, nil
	case country == "DE" && subdivision == "BY":
		return de.HolidaysBY, nil
	default:
		return nil, fmt.Errorf("unsupported holiday region (%s, %s); supported: SE, DE, DE/BY", country, subdivision)
	}
}

func (CountryHolidays) IsHoliday(ctx context.Context, date time.Time, country, subdivision string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	holidays, err := countryHolidays(country, subdivision)
	if err != nil {
		return false, err
	}
	for _, holiday := range holidays {
		if holiday.Type != cal.ObservancePublic {
			continue
		}
		actual, observed := holiday.Calc(date.Year())
		if sameDate(date, actual) || sameDate(date, observed) {
			return true, nil
		}
	}
	return false, nil
}

func sameDate(a, b time.Time) bool {
	if b.IsZero() {
		return false
	}
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

type observedHolidays struct {
	provider HolidayProvider
	metrics  *metrics
}

func (p observedHolidays) Version() string { return p.provider.Version() }

func (p observedHolidays) Validate(country, subdivision string) error {
	return p.provider.Validate(country, subdivision)
}

func (p observedHolidays) IsHoliday(ctx context.Context, date time.Time, country, subdivision string) (bool, error) {
	holiday, err := p.provider.IsHoliday(ctx, date, country, subdivision)
	result := "not_holiday"
	if holiday {
		result = "holiday"
	}
	if err != nil {
		result = "error"
	}
	p.metrics.holidays.WithLabelValues(country, result).Inc()
	return holiday, err
}
