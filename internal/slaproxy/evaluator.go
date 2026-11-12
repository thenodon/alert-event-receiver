package slaproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const (
	ReasonEligible         = "eligible"
	ReasonOutsideWindow    = "outside_window"
	ReasonHoliday          = "holiday"
	ReasonAdditionalDate   = "additional_date"
	ReasonHorizonExhausted = "no_eligibility_within_horizon"
)

// EvaluationError has a bounded operational reason, distinct from ineligibility.
type EvaluationError struct {
	Reason string
	Err    error
}

func (e *EvaluationError) Error() string { return e.Reason + ": " + e.Err.Error() }
func (e *EvaluationError) Unwrap() error { return e.Err }

type compiledPolicy struct {
	location                      *time.Location
	windows                       weeklyWindows
	additionalDates               map[string]bool
	country, subdivision, version string
}

// CalendarEngine contains immutable compiled policies and no occurrence state.
// The caller supplies the calendar instant. Webhook classification supplies the
// alert's startsAt; the processing clock and enrichment do not affect the result.
type CalendarEngine struct {
	policies    map[PolicyRef]compiledPolicy
	holidays    HolidayProvider
	horizonDays int
}

func NewCalendarEngine(cfg Config, holidays HolidayProvider) (*CalendarEngine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if holidays == nil {
		return nil, fmt.Errorf("holiday provider is required")
	}
	engine := &CalendarEngine{policies: make(map[PolicyRef]compiledPolicy), holidays: holidays, horizonDays: cfg.EvaluationHorizonDays}
	for _, def := range cfg.SLADefinitions {
		for name, policy := range def.SLAs {
			ref := PolicyRef{def.SLALabel, name}
			loc, err := time.LoadLocation(policy.Timezone)
			if err != nil {
				return nil, err
			}
			windows, err := compileSchedule(policy.Schedule)
			if err != nil {
				return nil, err
			}
			compiled := compiledPolicy{location: loc, windows: windows, additionalDates: map[string]bool{}}
			if h := policy.Holidays; h != nil {
				compiled.country, compiled.subdivision = h.Country, h.Subdivision
				if h.Country != "" {
					if err := holidays.Validate(h.Country, h.Subdivision); err != nil {
						return nil, fmt.Errorf("SLA (%s, %s): %w", ref.SLALabel, ref.SLAValue, err)
					}
				}
				for _, date := range h.AdditionalDates {
					compiled.additionalDates[date] = true
				}
			}
			// JSON sorts map keys, so policy versions do not depend on YAML map
			// order. Include selection namespace, search bounds and rule versions.
			encoded, err := json.Marshal(struct {
				Ref             PolicyRef
				Policy          Policy
				HorizonDays     int
				Provider, Rules string
			}{ref, policy, cfg.EvaluationHorizonDays, holidays.Version(), "wall-clock-v1"})
			if err != nil {
				return nil, err
			}
			digest := sha256.Sum256(encoded)
			compiled.version = hex.EncodeToString(digest[:])
			engine.policies[ref] = compiled
		}
	}
	return engine, nil
}

// Evaluate returns the first eligible instant at or after at. When eligible now,
// NextEvaluationAt is at. Zero next time means no instant was found within the
// horizon, not permanent ineligibility. sinkID is reserved for future routing;
// Phase 2 evaluates the supplied PolicyRef identically for every caller.
func (e *CalendarEngine) Evaluate(ctx context.Context, _ PolicyInput, ref PolicyRef, _ string, at time.Time) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, &EvaluationError{"cancelled", err}
	}
	p, ok := e.policies[ref]
	if !ok {
		return Decision{}, &EvaluationError{"unknown_policy", fmt.Errorf("policy reference is not configured")}
	}
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return Decision{}, &EvaluationError{"invalid_evaluation_time", fmt.Errorf("a valid evaluation instant is required")}
	}
	local := at.In(p.location)
	y, m, d := local.Date()
	// UTC here is a coordinate for civil dates, never the instant of local
	// midnight. AddDate advances calendar dates across DST and skipped dates.
	date := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	decision := Decision{Reason: ReasonOutsideWindow, PolicyVersion: p.version}
	for day := 0; day < e.horizonDays && date.Year() <= 9999; day++ {
		if err := ctx.Err(); err != nil {
			return Decision{}, &EvaluationError{"cancelled", err}
		}
		windows := p.windows[date.Weekday()]
		if len(windows) != 0 || day == 0 {
			excluded, reason, err := e.excluded(ctx, p, date)
			if err != nil {
				return Decision{}, &EvaluationError{"calendar_error", err}
			}
			if day == 0 && excluded {
				decision.Reason = reason
			}
			if !excluded {
				next := nextOnDate(date, p.location, windows, at)
				if !next.IsZero() {
					decision.NextEvaluationAt = next.UTC()
					decision.Eligible = next.Equal(at)
					if decision.Eligible {
						decision.Reason = ReasonEligible
					}
					return decision, nil
				}
			}
		}
		date = date.AddDate(0, 0, 1)
	}
	decision.Reason = ReasonHorizonExhausted
	return decision, nil
}

func (e *CalendarEngine) excluded(ctx context.Context, p compiledPolicy, date time.Time) (bool, string, error) {
	if p.additionalDates[date.Format("2006-01-02")] {
		return true, ReasonAdditionalDate, nil
	}
	if p.country != "" {
		holiday, err := e.holidays.IsHoliday(ctx, date, p.country, p.subdivision)
		return holiday, ReasonHoliday, err
	}
	return false, "", nil
}

// nextOnDate maps wall-clock intervals to real UTC intervals using each zone
// offset segment. It neither guesses which fold time.Date chooses nor normalizes
// nonexistent times. A gap has no instants; a fold yields both occurrences.
func nextOnDate(date time.Time, loc *time.Location, windows []minuteInterval, at time.Time) time.Time {
	if len(windows) == 0 {
		return time.Time{}
	}
	var best time.Time
	// IANA offsets are within 24 hours. The wider envelope includes all real
	// instants of this civil date, including dateline changes and midnight DST.
	limit := date.Add(72 * time.Hour)
	for cursor := date.Add(-48 * time.Hour); cursor.Before(limit); {
		zoned := cursor.In(loc)
		_, offset := zoned.Zone()
		_, zoneEnd := zoned.ZoneBounds()
		end := limit
		if !zoneEnd.IsZero() && zoneEnd.Before(end) {
			end = zoneEnd
		}
		for _, window := range windows {
			start := date.Add(time.Duration(window.start)*time.Minute - time.Duration(offset)*time.Second)
			stop := date.Add(time.Duration(window.end)*time.Minute - time.Duration(offset)*time.Second)
			if start.Before(cursor) {
				start = cursor
			}
			if start.Before(at) {
				start = at
			}
			if stop.After(end) {
				stop = end
			}
			if start.Before(stop) && (best.IsZero() || start.Before(best)) {
				best = start
			}
		}
		cursor = end
	}
	return best
}

var _ PolicyEvaluator = (*CalendarEngine)(nil)
