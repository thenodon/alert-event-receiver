package slaproxy

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

const (
	workDue            = "due"
	workScheduled      = "scheduled"
	workRetryScheduled = "retry_scheduled"
	workClaimed        = "claimed"
	workClaimExpired   = "claim_expired"
	workHeld           = "held"
)

type deliveryWorkItem struct {
	OccurrenceID   string     `json:"occurrence_id"`
	OccurrenceURL  string     `json:"occurrence_url"`
	BindingID      string     `json:"binding_id"`
	SinkID         string     `json:"sink_id"`
	IntentID       string     `json:"intent_id"`
	Action         string     `json:"action"`
	State          string     `json:"state"`
	Attempts       uint64     `json:"attempts"`
	HoldReason     string     `json:"hold_reason,omitempty"`
	LastFailure    string     `json:"last_failure,omitempty"`
	DueAt          *time.Time `json:"due_at,omitempty"`
	NextAttemptAt  *time.Time `json:"next_attempt_at,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
}

func (s *Service) inspectDeliveryWork(w http.ResponseWriter, r *http.Request) {
	page, ok := readInspectionPage(w, r, s, true, deliveryInspectionItems)
	if ok {
		writeInspectionJSON(w, struct {
			inspectionPage[deliveryWorkItem]
			DeliveryEnabled bool `json:"delivery_enabled"`
		}{page, s.cfg.Delivery.Enabled})
	}
}

func deliveryInspectionItems(_ context.Context, q url.Values, row *DurableOccurrence, asOf time.Time) []deliveryWorkItem {
	var items []deliveryWorkItem
	for _, b := range orderedInspectionBindings(row) {
		if !matchesInspectionFilter(q.Get("sink_id"), b.SinkID) {
			continue
		}
		for _, action := range []string{"firing", "resolved"} {
			intent := b.Intents[action]
			if intent == nil || !matchesInspectionFilter(q.Get("action"), action) {
				continue
			}
			item := inspectDeliveryIntent(row, b, intent, asOf)
			if item.State != "" && matchesInspectionFilter(q.Get("state"), item.State) {
				items = append(items, item)
			}
		}
	}
	return items
}

func inspectDeliveryIntent(row *DurableOccurrence, b *DurableBinding, i *DeliveryIntent, asOf time.Time) deliveryWorkItem {
	item := deliveryWorkItem{
		OccurrenceID: row.ID, OccurrenceURL: inspectionPath + "/" + row.ID,
		BindingID: b.ID, SinkID: b.SinkID, IntentID: i.ID, Action: i.Action,
		Attempts: i.Attempts, LastFailure: i.LastFailure, HoldReason: inspectionHoldReason(b),
	}
	switch {
	case b.Claim != nil && b.Claim.IntentID == i.ID:
		item.State = workClaimed
		item.LeaseExpiresAt = utcPointer(b.Claim.Until)
		if !b.Claim.Until.After(asOf) {
			item.State = workClaimExpired
		}
	case i.Receipt != nil || i.Canceled:
	case i.PermanentFailure || b.HoldReason != "":
		item.State = workHeld
	case deliveryActionable(row, b, i):
		due := i.dueAt()
		item.DueAt = utcPointer(due)
		item.NextAttemptAt = utcPointer(i.NextAttemptAt)
		switch {
		case !due.After(asOf):
			item.State = workDue
		case !i.NextAttemptAt.IsZero():
			item.State = workRetryScheduled
		default:
			item.State = workScheduled
		}
	}
	return item
}
