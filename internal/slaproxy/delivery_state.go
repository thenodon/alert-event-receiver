package slaproxy

import "time"

func (o *DurableOccurrence) outstandingDelivery() bool {
	if o == nil {
		return false
	}
	for _, b := range o.Bindings {
		if b.Claim != nil {
			return true
		}
		for _, i := range b.Intents {
			if i.Receipt != nil {
				continue
			}
			if i.AuthorizedAt != nil || (!i.Canceled && (i.Ready || i.PermanentFailure || (i.Action == "firing" && o.Lifecycle == LifecycleFiring && b.Eligibility == EligibilityEligible) || (i.Action == "resolved" && b.FiringDeliveredAt != nil && o.Lifecycle == LifecycleResolved))) {
				return true
			}
		}
	}
	return false
}

// refreshDelivery derives readiness and the binding summary from existing
// lifecycle/eligibility and per-action receipts. It is not a second lifecycle FSM.
func (m *DurableManager) refreshDelivery(row *DurableOccurrence) {
	for _, b := range row.Bindings {
		firing := b.Intents["firing"]
		if firing != nil {
			switch {
			case firing.Receipt != nil:
				firing.Ready, firing.Canceled = false, false
			case row.Lifecycle == LifecycleResolved && firing.AuthorizedAt == nil:
				firing.Ready, firing.Canceled = false, true
			default:
				firing.Canceled = false
				firing.Ready = !firing.PermanentFailure && b.HoldReason == "" && (b.Eligibility == EligibilityEligible || firing.AuthorizedAt != nil)
			}
		}
		resolved := b.Intents["resolved"]
		if resolved != nil {
			resolved.Ready = resolved.Receipt == nil && !resolved.PermanentFailure && row.Lifecycle == LifecycleResolved && b.FiringDeliveredAt != nil && b.HoldReason == ""
		}
		b.Delivery = NotDelivered
		var current *DeliveryIntent
		if row.Lifecycle == LifecycleResolved && b.FiringDeliveredAt != nil {
			current = resolved
		} else {
			current = firing
		}
		if current != nil {
			switch {
			case current.Receipt != nil:
				b.Delivery = Delivered
			case current.Canceled && current.AuthorizedAt == nil:
				b.Delivery = NotDelivered
			case current.PermanentFailure || current.LastFailure != "":
				b.Delivery = DeliveryFailed
			case current.Ready:
				b.Delivery = PendingDelivery
			}
		}
	}
}

func (b *DurableBinding) deliveryDueAt(row *DurableOccurrence) time.Time {
	if b.Claim != nil {
		return b.Claim.Until
	}
	var due time.Time
	for _, action := range []string{"firing", "resolved"} {
		i := b.Intents[action]
		if !deliveryActionable(row, b, i) {
			continue
		}
		at := i.NextAttemptAt
		if at.IsZero() {
			at = i.CreatedAt
		}
		if due.IsZero() || at.Before(due) {
			due = at
		}
	}
	return due
}
func deliveryActionable(row *DurableOccurrence, b *DurableBinding, i *DeliveryIntent) bool {
	if i == nil || !i.Ready || i.Canceled || i.PermanentFailure || i.Receipt != nil || b.HoldReason != "" {
		return false
	}
	if i.Action == "resolved" {
		return row.Lifecycle == LifecycleResolved && b.FiringDeliveredAt != nil
	}
	if i.Action != "firing" || b.FiringDeliveredAt != nil {
		return false
	}
	// A previously authorized send may have succeeded without its receipt. Such
	// recovery retries are not retroactive first deliveries for resolved alerts.
	return (row.Lifecycle == LifecycleFiring && b.Eligibility == EligibilityEligible) || i.AuthorizedAt != nil
}

func (b *DurableBinding) deliveryHeld() bool {
	for _, i := range b.Intents {
		if i.Receipt == nil && !i.Canceled && (i.PermanentFailure || b.HoldReason != "") {
			return true
		}
	}
	return false
}
