package slaproxy

import (
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
)

type EligibilityStatus string

const (
	EligibilityEligible   EligibilityStatus = "eligible"
	EligibilityIneligible EligibilityStatus = "ineligible"
	EligibilityPending    EligibilityStatus = "pending"
	EligibilityUnknown    EligibilityStatus = "unknown"
	EligibilityError      EligibilityStatus = "error"
)

// alertEligibility consumes a successful calendar decision at alert.StartsAt.
// A firing notification establishes activity at its start, not continuous
// activity until reception. Only resolved notifications bound the full interval.
func alertEligibility(alert models.Alert, calendar Decision, cutoff time.Time) (EligibilityStatus, string) {
	first := calendar.NextEvaluationAt
	if alert.Status == "resolved" {
		if alert.EndsAt.Equal(alert.StartsAt) {
			return EligibilityIneligible, "empty_active_interval"
		}
		if !first.IsZero() {
			if first.Before(alert.EndsAt) {
				return EligibilityEligible, "active_interval_overlaps_sla"
			}
			return EligibilityIneligible, "resolved_before_first_eligible_instant"
		}
		if !calendar.SearchUntil.IsZero() && !alert.EndsAt.After(calendar.SearchUntil) {
			return EligibilityIneligible, "resolved_interval_searched"
		}
		return EligibilityUnknown, "search_horizon_exhausted"
	}
	if alert.StartsAt.After(cutoff) {
		return EligibilityUnknown, "start_after_observation_cutoff"
	}
	if calendar.Eligible {
		return EligibilityEligible, "start_in_sla"
	}
	if first.IsZero() {
		return EligibilityUnknown, "search_horizon_exhausted"
	}
	if first.After(cutoff) {
		return EligibilityPending, "awaiting_first_eligible_instant"
	}
	return EligibilityUnknown, "activity_at_eligibility_unverified"
}
