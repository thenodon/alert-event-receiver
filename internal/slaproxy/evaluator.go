package slaproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const (
	calendarRulesVersion   = "wall-clock-v1"
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
	location        *time.Location
	windows         weeklyWindows
	additionalDates map[string]bool
	version         string
	dataset         *holidayDataset
}

// CalendarEngine contains immutable compiled policies and no occurrence state.
// The caller supplies the calendar instant. Webhook classification supplies the
// alert's startsAt; the processing clock and enrichment do not affect the result.
type CalendarEngine struct {
	policies    map[PolicyRef]compiledPolicy
	metrics     *metrics
	horizonDays int
}

func NewCalendarEngine(cfg Config) (*CalendarEngine, error) {
	return newCalendarEngineAt(cfg, time.Now())
}

// The clock is injectable for boundary tests; dataset data itself is immutable.
func newCalendarEngineAt(cfg Config, now time.Time) (*CalendarEngine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	engine := &CalendarEngine{policies: make(map[PolicyRef]compiledPolicy), horizonDays: cfg.EvaluationHorizonDays}
	cache := make(map[string]*holidayDataset)
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
				if h.Dataset != nil {
					path, err := canonicalDatasetPath(*h.Dataset)
					if err != nil {
						return nil, fmt.Errorf("SLA (%s, %s): %w", ref.SLALabel, ref.SLAValue, err)
					}
					dataset := cache[path]
					if dataset == nil {
						dataset, err = loadHolidayDataset(path)
						if err != nil {
							return nil, fmt.Errorf("SLA (%s, %s): %w", ref.SLALabel, ref.SLAValue, err)
						}
						cache[path] = dataset
					}
					compiled.dataset = dataset
				}
				for _, date := range h.AdditionalDates {
					compiled.additionalDates[date] = true
				}
			}
			// Hash effective semantics, never file paths or provider provenance.
			effective := policy
			datasetHash := ""
			if policy.Holidays != nil {
				normalized := *policy.Holidays
				normalized.Dataset = nil
				normalized.AdditionalDates = make([]string, 0, len(compiled.additionalDates))
				for date := range compiled.additionalDates {
					normalized.AdditionalDates = append(normalized.AdditionalDates, date)
				}
				sort.Strings(normalized.AdditionalDates)
				effective.Holidays = &normalized
			}
			if compiled.dataset != nil {
				datasetHash = compiled.dataset.semanticHash
			}
			encoded, err := json.Marshal(struct {
				Ref            PolicyRef
				Policy         Policy
				HorizonDays    int
				Dataset, Rules string
			}{ref, effective, cfg.EvaluationHorizonDays, datasetHash, calendarRulesVersion})
			if err != nil {
				return nil, err
			}
			digest := sha256.Sum256(encoded)
			compiled.version = hex.EncodeToString(digest[:])
			engine.policies[ref] = compiled
		}
	}
	if err := engine.coverageAt(now); err != nil {
		return nil, err
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
	if p.dataset != nil && !p.dataset.covers(date) {
		e.observeDataset(false, errDatasetCoverage)
		return Decision{}, &EvaluationError{"calendar_error", errDatasetCoverage}
	}
	decision := Decision{Reason: ReasonOutsideWindow, PolicyVersion: p.version}
	for day := 0; day < e.horizonDays && date.Year() <= 9999; day++ {
		if err := ctx.Err(); err != nil {
			return Decision{}, &EvaluationError{"cancelled", err}
		}
		// Coverage applies even on unscheduled days and explicit additions.
		if p.dataset != nil && !p.dataset.covers(date) {
			e.observeDataset(false, errDatasetCoverage)
			return Decision{}, &EvaluationError{"calendar_error", errDatasetCoverage}
		}
		windows := p.windows[date.Weekday()]
		if len(windows) != 0 || day == 0 {
			excluded, reason, err := e.excluded(p, date)
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
	// Expose the exhausted range without changing the window search. Use the
	// existing instant projection for midnight gaps/folds and skipped dates.
	for skipped := 0; skipped < 3 && date.Year() <= 9999; skipped++ {
		boundary := nextOnDate(date, p.location, []minuteInterval{{0, 1440}}, at)
		if !boundary.IsZero() {
			decision.SearchUntil = boundary.UTC()
			break
		}
		date = date.AddDate(0, 0, 1)
	}
	return decision, nil
}

func (e *CalendarEngine) excluded(p compiledPolicy, date time.Time) (bool, string, error) {
	if p.dataset != nil && !p.dataset.covers(date) {
		e.observeDataset(false, errDatasetCoverage)
		return false, "", errDatasetCoverage
	}
	if p.additionalDates[date.Format("2006-01-02")] {
		return true, ReasonAdditionalDate, nil
	}
	if p.dataset != nil {
		holiday := p.dataset.dates[date.Format("2006-01-02")]
		e.observeDataset(holiday, nil)
		return holiday, ReasonHoliday, nil
	}
	return false, "", nil
}

// A rolling readiness window gives operators horizon-length warning before the
// finite data ends. Per-evaluation guards still cover historical/future inputs.
// Boundary allowance includes the previous civil day and three future days.
func (e *CalendarEngine) coverageAt(now time.Time) error {
	for ref, p := range e.policies {
		if p.dataset == nil {
			continue
		}
		local := now.In(p.location)
		date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
		if !p.dataset.covers(date.AddDate(0, 0, -1)) || !p.dataset.covers(date.AddDate(0, 0, e.horizonDays+3)) {
			return fmt.Errorf("SLA (%s, %s): ICS coverage must include yesterday through today + horizon + 3 local dates", ref.SLALabel, ref.SLAValue)
		}
	}
	return nil
}

func (e *CalendarEngine) observeDataset(holiday bool, err error) {
	if e.metrics == nil {
		return
	}
	result := "not_holiday"
	if holiday {
		result = "holiday"
	}
	if err != nil {
		result = "error"
	}
	e.metrics.holidays.WithLabelValues(result).Inc()
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
