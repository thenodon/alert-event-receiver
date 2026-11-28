package slaproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	windowOpen    = "open"
	windowClosed  = "closed"
	windowUnknown = "unknown"

	inspectionPath            = "/api/v1/occurrences"
	inspectionDeliveryPath    = "/api/v1/delivery-work"
	inspectionScanCount       = 32
	inspectionScanBudget      = 16
	inspectionCandidateBudget = 256
)

var inspectionOccurrenceID = regexp.MustCompile("^[a-f0-9]{64}$")

func inspectionPathConflict(path string) bool {
	if path == inspectionPath || path == inspectionDeliveryPath {
		return true
	}
	id, ok := strings.CutPrefix(path, inspectionPath+"/")
	return ok && !strings.Contains(id, "/")
}

func (s *Service) registerInspection(mux *http.ServeMux) {
	register := func(path string, handler http.HandlerFunc) {
		protected := s.security.Inspection(handler)
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			protected.ServeHTTP(w, r)
		})
	}
	register(inspectionPath, s.inspectOccurrences)
	register(inspectionPath+"/{occurrence_id}", s.inspectOccurrence)
	register(inspectionDeliveryPath, s.inspectDeliveryWork)
}

type occurrenceView struct {
	ID          string          `json:"occurrence_id"`
	AlertName   string          `json:"alertname,omitempty"`
	Fingerprint string          `json:"fingerprint"`
	Lifecycle   LifecycleStatus `json:"lifecycle"`
	StartsAt    time.Time       `json:"starts_at"`
	EndsAt      *time.Time      `json:"ends_at,omitempty"`
}

type occurrenceBindingView struct {
	ID               string            `json:"binding_id"`
	SinkID           string            `json:"sink_id"`
	Eligibility      EligibilityStatus `json:"eligibility"`
	Delivery         DeliveryStatus    `json:"delivery"`
	SLAWindow        string            `json:"sla_window"`
	SLAWindowReason  string            `json:"sla_window_reason,omitempty"`
	HoldReason       string            `json:"hold_reason,omitempty"`
	NextEvaluationAt *time.Time        `json:"next_evaluation_at,omitempty"`
}

type occurrenceListItem struct {
	occurrenceView
	Bindings []occurrenceBindingView `json:"bindings"`
}

type occurrenceDetail struct {
	AsOf time.Time `json:"as_of"`
	occurrenceView
	Version             uint64                    `json:"version"`
	CreatedAt           time.Time                 `json:"created_at"`
	UpdatedAt           time.Time                 `json:"updated_at"`
	LastEvaluatedAt     *time.Time                `json:"last_evaluated_at,omitempty"`
	CompletedAt         *time.Time                `json:"completed_at,omitempty"`
	CompactAt           *time.Time                `json:"compact_at,omitempty"`
	DeleteAt            *time.Time                `json:"delete_at,omitempty"`
	Compacted           bool                      `json:"compacted"`
	ResolutionConflict  bool                      `json:"resolution_conflict"`
	SchedulerLeaseUntil *time.Time                `json:"scheduler_lease_until,omitempty"`
	Bindings            []occurrenceBindingDetail `json:"bindings"`
}

type occurrenceBindingDetail struct {
	occurrenceBindingView
	SLALabel           string             `json:"sla_label"`
	SLAValue           string             `json:"sla_value"`
	PolicyVersion      string             `json:"policy_version"`
	DeliveryLeaseUntil *time.Time         `json:"delivery_lease_until,omitempty"`
	Intents            []inspectionIntent `json:"intents"`
}

type inspectionIntent struct {
	ID                 string     `json:"intent_id"`
	Action             string     `json:"action"`
	Ready              bool       `json:"ready"`
	Canceled           bool       `json:"canceled"`
	AuthorizedAt       *time.Time `json:"authorized_at,omitempty"`
	Attempts           uint64     `json:"attempts"`
	LastFailure        string     `json:"last_failure,omitempty"`
	NextAttemptAt      *time.Time `json:"next_attempt_at,omitempty"`
	ReceiptCommittedAt *time.Time `json:"receipt_committed_at,omitempty"`
}

func inspectOccurrenceView(row *DurableOccurrence) occurrenceView {
	view := occurrenceView{
		ID: row.ID, Fingerprint: row.Fingerprint, Lifecycle: row.Lifecycle,
		StartsAt: row.StartsAt.UTC(), EndsAt: inspectionTime(row.EndsAt),
	}
	for _, payload := range []*Notification{row.Original, row.LastFiring, row.Resolution} {
		if payload != nil && payload.Alert.Labels["alertname"] != "" {
			view.AlertName = payload.Alert.Labels["alertname"]
			break
		}
	}
	return view
}

func inspectionTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	return utcPointer(*t)
}

func (e *CalendarEngine) inspectionWindow(ctx context.Context, b *DurableBinding, at time.Time) (window, reason string) {
	policy, ok := e.policies[b.Policy]
	if !ok {
		return windowUnknown, "policy_removed"
	}
	if policy.version != b.PolicyVersion {
		return windowUnknown, "policy_changed"
	}
	// Evaluate only the current civil date and leave operational metrics to
	// scheduler and webhook evaluations.
	calendar := *e
	calendar.horizonDays, calendar.metrics = 1, nil
	decision, err := calendar.Evaluate(ctx, PolicyInput{}, b.Policy, b.SinkID, at)
	if errors.Is(err, errDatasetCoverage) {
		return windowUnknown, "holiday_coverage_missing"
	}
	if err != nil {
		return windowUnknown, "calendar_error"
	}
	if decision.Eligible {
		return windowOpen, ""
	}
	return windowClosed, ""
}

func (e *CalendarEngine) inspectBinding(ctx context.Context, b *DurableBinding, at time.Time) occurrenceBindingView {
	window, reason := e.inspectionWindow(ctx, b, at)
	return occurrenceBindingView{
		ID: b.ID, SinkID: b.SinkID, Eligibility: b.Eligibility, Delivery: b.Delivery,
		SLAWindow: window, SLAWindowReason: reason, HoldReason: inspectionHoldReason(b), NextEvaluationAt: utcPointer(b.NextEvaluationAt),
	}
}

func inspectionHoldReason(b *DurableBinding) string {
	if resolved := b.Intents["resolved"]; resolved != nil && resolved.Receipt != nil {
		return ""
	}
	return b.HoldReason
}

func orderedInspectionBindings(row *DurableOccurrence) []*DurableBinding {
	bindings := make([]*DurableBinding, 0, len(row.Bindings))
	for _, b := range row.Bindings {
		bindings = append(bindings, b)
	}
	slices.SortFunc(bindings, func(a, b *DurableBinding) int { return strings.Compare(a.ID, b.ID) })
	return bindings
}

func (s *Service) inspectOccurrence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("occurrence_id")
	if !inspectionOccurrenceID.MatchString(id) {
		http.Error(w, "invalid occurrence ID", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Durable.Scheduler.OperationTimeout)
	defer cancel()
	row, err := s.durable.store.load(ctx, id)
	if err != nil {
		http.Error(w, "occurrence state unavailable", http.StatusServiceUnavailable)
		return
	}
	if row == nil {
		http.NotFound(w, r)
		return
	}
	asOf := s.now().UTC()
	result := occurrenceDetail{
		AsOf:           asOf,
		occurrenceView: inspectOccurrenceView(row), Version: row.Version,
		CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
		LastEvaluatedAt: inspectionTime(row.LastEvaluatedAt), CompletedAt: inspectionTime(row.CompletedAt),
		CompactAt: inspectionTime(row.CompactAt), DeleteAt: inspectionTime(row.DeleteAt),
		Compacted: row.Compacted, ResolutionConflict: row.ResolutionConflict,
		Bindings: make([]occurrenceBindingDetail, 0, len(row.Bindings)),
	}
	if row.Claim != nil {
		result.SchedulerLeaseUntil = utcPointer(row.Claim.Until)
	}
	for _, b := range orderedInspectionBindings(row) {
		detail := occurrenceBindingDetail{
			occurrenceBindingView: s.durable.engine.inspectBinding(ctx, b, asOf), SLALabel: b.Policy.SLALabel, SLAValue: b.Policy.SLAValue,
			PolicyVersion: b.PolicyVersion, Intents: make([]inspectionIntent, 0, len(b.Intents)),
		}
		if b.Claim != nil {
			detail.DeliveryLeaseUntil = utcPointer(b.Claim.Until)
		}
		for _, action := range []string{"firing", "resolved"} {
			if i := b.Intents[action]; i != nil {
				intent := inspectionIntent{
					ID: i.ID, Action: action, Ready: i.Ready, Canceled: i.Canceled,
					AuthorizedAt: inspectionTime(i.AuthorizedAt), Attempts: i.Attempts,
					LastFailure: i.LastFailure, NextAttemptAt: utcPointer(i.NextAttemptAt),
				}
				if i.Receipt != nil {
					intent.ReceiptCommittedAt = utcPointer(i.Receipt.CommittedAt)
				}
				detail.Intents = append(detail.Intents, intent)
			}
		}
		result.Bindings = append(result.Bindings, detail)
	}
	if ctx.Err() != nil {
		http.Error(w, "occurrence state unavailable", http.StatusServiceUnavailable)
		return
	}
	writeInspectionJSON(w, result)
}

type inspectionCursor struct {
	Scope   string    `json:"scope"`
	Next    uint64    `json:"next"`
	Pending []string  `json:"pending,omitempty"`
	Offset  int       `json:"offset,omitempty"`
	AsOf    time.Time `json:"as_of"`
}

type inspectionPage[T any] struct {
	Items      []T       `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
	AsOf       time.Time `json:"as_of"`
}

func inspectionFilters(raw string, deliveryWork bool) (url.Values, int, bool) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, 0, false
	}
	limit := 25
	for key, values := range q {
		if len(values) != 1 || values[0] == "" {
			return nil, 0, false
		}
		value := values[0]
		switch key {
		case "cursor", "limit", "sink_id":
		case "action", "state":
			if !deliveryWork {
				return nil, 0, false
			}
		default:
			if deliveryWork {
				return nil, 0, false
			}
		}
		switch key {
		case "alertname", "fingerprint", "sink_id", "hold_reason", "cursor":
		case "sla_window":
			if value != windowOpen && value != windowClosed && value != windowUnknown {
				return nil, 0, false
			}
		case "action":
			if value != "firing" && value != "resolved" {
				return nil, 0, false
			}
		case "state":
			switch value {
			case workDue, workRetryScheduled, workClaimed, workClaimExpired, workHeld, workScheduled:
			default:
				return nil, 0, false
			}
		case "lifecycle":
			if value != string(LifecycleFiring) && value != string(LifecycleResolved) {
				return nil, 0, false
			}
		case "eligibility":
			switch EligibilityStatus(value) {
			case EligibilityEligible, EligibilityIneligible, EligibilityPending, EligibilityUnknown:
			default:
				return nil, 0, false
			}
		case "delivery":
			switch DeliveryStatus(value) {
			case NotDelivered, PendingDelivery, Delivered, DeliveryFailed:
			default:
				return nil, 0, false
			}
		case "limit":
			limit, err = strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 100 {
				return nil, 0, false
			}
		default:
			return nil, 0, false
		}
	}
	return q, limit, true
}

func matchesInspectionFilter(filter, value string) bool {
	return filter == "" || filter == value
}

func (s *Service) occurrenceInspectionItems(ctx context.Context, q url.Values, row *DurableOccurrence, asOf time.Time) []occurrenceListItem {
	view := inspectOccurrenceView(row)
	if !matchesInspectionFilter(q.Get("lifecycle"), string(view.Lifecycle)) ||
		!matchesInspectionFilter(q.Get("alertname"), view.AlertName) ||
		!matchesInspectionFilter(q.Get("fingerprint"), view.Fingerprint) {
		return nil
	}
	item := occurrenceListItem{occurrenceView: view, Bindings: make([]occurrenceBindingView, 0, len(row.Bindings))}
	matched := false
	for _, b := range orderedInspectionBindings(row) {
		binding := s.durable.engine.inspectBinding(ctx, b, asOf)
		item.Bindings = append(item.Bindings, binding)
		if matchesInspectionFilter(q.Get("sink_id"), binding.SinkID) &&
			matchesInspectionFilter(q.Get("eligibility"), string(binding.Eligibility)) &&
			matchesInspectionFilter(q.Get("delivery"), string(binding.Delivery)) &&
			matchesInspectionFilter(q.Get("hold_reason"), binding.HoldReason) &&
			matchesInspectionFilter(q.Get("sla_window"), binding.SLAWindow) {
			matched = true
		}
	}
	if !matched {
		return nil
	}
	return []occurrenceListItem{item}
}

func (s *Service) inspectOccurrences(w http.ResponseWriter, r *http.Request) {
	page, ok := readInspectionPage(w, r, s, false, s.occurrenceInspectionItems)
	if ok {
		writeInspectionJSON(w, page)
	}
}

func readInspectionPage[T any](w http.ResponseWriter, r *http.Request, s *Service, deliveryWork bool,
	items func(context.Context, url.Values, *DurableOccurrence, time.Time) []T) (inspectionPage[T], bool) {
	var empty inspectionPage[T]
	q, limit, valid := inspectionFilters(r.URL.RawQuery, deliveryWork)
	if !valid {
		http.Error(w, "invalid inspection filters", http.StatusBadRequest)
		return empty, false
	}
	token := q.Get("cursor")
	q.Del("cursor")
	q.Del("limit")
	store := s.durable.store
	scope := tupleID(store.prefix, strconv.Itoa(store.cfg.Valkey.Database), q.Encode())
	if deliveryWork {
		scope = tupleID(inspectionDeliveryPath, scope)
	}
	cursor := inspectionCursor{Scope: scope, AsOf: s.now().UTC()}
	if token != "" {
		data, err := base64.RawURLEncoding.DecodeString(token)
		var resumed inspectionCursor
		if err != nil || json.Unmarshal(data, &resumed) != nil || resumed.Scope != scope ||
			resumed.Offset < 0 || (!deliveryWork && resumed.Offset != 0) ||
			(len(resumed.Pending) == 0 && (resumed.Next == 0 || resumed.Offset != 0)) ||
			slices.ContainsFunc(resumed.Pending, func(id string) bool { return !inspectionOccurrenceID.MatchString(id) }) {
			http.Error(w, "invalid inspection cursor", http.StatusBadRequest)
			return empty, false
		}
		if resumed.AsOf.IsZero() {
			resumed.AsOf = cursor.AsOf // Phase 1 cursors have no timestamp.
		}
		resumed.AsOf = resumed.AsOf.UTC()
		cursor = resumed
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Durable.Scheduler.OperationTimeout)
	defer cancel()
	firing := q.Get("lifecycle") == string(LifecycleFiring)
	page, err := inspectPage(ctx, store, firing, limit, cursor, token == "", func(row *DurableOccurrence) []T {
		return items(ctx, q, row, cursor.AsOf)
	})
	if err != nil {
		http.Error(w, "occurrence state unavailable", http.StatusServiceUnavailable)
		return empty, false
	}
	return page, true
}

func inspectPage[T any](ctx context.Context, store *DurableStore, firing bool, limit int, cursor inspectionCursor,
	initial bool, inspect func(*DurableOccurrence) []T) (inspectionPage[T], error) {
	page := inspectionPage[T]{Items: make([]T, 0, limit), AsOf: cursor.AsOf}
	if err := store.checkSchema(ctx, store.client); err != nil {
		return page, err
	}
	candidates, scans := 0, 0
	for len(page.Items) < limit && candidates < inspectionCandidateBudget {
		if len(cursor.Pending) == 0 {
			if (!initial && cursor.Next == 0) || scans == inspectionScanBudget {
				break
			}
			ids, next, err := store.scanInspection(ctx, firing, cursor.Next)
			if err != nil {
				return page, err
			}
			cursor.Next, cursor.Pending = next, ids
			scans++
			initial = false
			continue
		}
		row, err := store.load(ctx, cursor.Pending[0])
		if err != nil {
			return page, err
		}
		candidates++
		if row != nil {
			items := inspect(row)
			if cursor.Offset < len(items) {
				end := min(len(items), cursor.Offset+limit-len(page.Items))
				page.Items = append(page.Items, items[cursor.Offset:end]...)
				if end < len(items) {
					cursor.Offset = end
					break
				}
			}
		}
		cursor.Pending, cursor.Offset = cursor.Pending[1:], 0
	}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	if cursor.Next != 0 || len(cursor.Pending) != 0 {
		// SCAN's COUNT is a hint. Preserve unconsumed candidates and item offset.
		data, err := json.Marshal(cursor)
		if err != nil {
			return page, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	return page, nil
}

func (s *DurableStore) scanInspection(ctx context.Context, firing bool, cursor uint64) ([]string, uint64, error) {
	if firing {
		ids, next, err := s.client.SScan(ctx, s.key("unresolved"), cursor, "*", inspectionScanCount).Result()
		return slices.DeleteFunc(ids, func(id string) bool { return !inspectionOccurrenceID.MatchString(id) }), next, err
	}
	prefix := s.key("occ:")
	keys, next, err := s.client.Scan(ctx, cursor, prefix+"*", inspectionScanCount).Result()
	for i := range keys {
		keys[i] = strings.TrimPrefix(keys[i], prefix)
	}
	return slices.DeleteFunc(keys, func(id string) bool { return !inspectionOccurrenceID.MatchString(id) }), next, err
}

func writeInspectionJSON(w http.ResponseWriter, response any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
