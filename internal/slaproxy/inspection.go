package slaproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	inspectionPath            = "/api/v1/occurrences"
	inspectionScanCount       = 32
	inspectionScanBudget      = 16
	inspectionCandidateBudget = 256
)

var inspectionOccurrenceID = regexp.MustCompile("^[a-f0-9]{64}$")

func inspectionPathConflict(path string) bool {
	if path == inspectionPath {
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
	HoldReason       string            `json:"hold_reason,omitempty"`
	NextEvaluationAt *time.Time        `json:"next_evaluation_at,omitempty"`
}

type occurrenceListItem struct {
	occurrenceView
	Bindings []occurrenceBindingView `json:"bindings"`
}

type occurrenceDetail struct {
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

func inspectBinding(b *DurableBinding) occurrenceBindingView {
	return occurrenceBindingView{
		ID: b.ID, SinkID: b.SinkID, Eligibility: b.Eligibility, Delivery: b.Delivery,
		HoldReason: inspectionHoldReason(b), NextEvaluationAt: utcPointer(b.NextEvaluationAt),
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
	result := occurrenceDetail{
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
			occurrenceBindingView: inspectBinding(b), SLALabel: b.Policy.SLALabel, SLAValue: b.Policy.SLAValue,
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
	writeInspectionJSON(w, result)
}

type inspectionCursor struct {
	Scope   string   `json:"scope"`
	Next    uint64   `json:"next"`
	Pending []string `json:"pending,omitempty"`
}

type inspectionPage struct {
	Items      []occurrenceListItem `json:"items"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

func inspectionFilters(raw string) (url.Values, int, bool) {
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
		case "alertname", "fingerprint", "sink_id", "hold_reason", "cursor":
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

func matchesInspection(q url.Values, view occurrenceView, row *DurableOccurrence) bool {
	if !matchesInspectionFilter(q.Get("lifecycle"), string(view.Lifecycle)) ||
		!matchesInspectionFilter(q.Get("alertname"), view.AlertName) ||
		!matchesInspectionFilter(q.Get("fingerprint"), view.Fingerprint) {
		return false
	}
	for _, b := range row.Bindings {
		if matchesInspectionFilter(q.Get("sink_id"), b.SinkID) &&
			matchesInspectionFilter(q.Get("eligibility"), string(b.Eligibility)) &&
			matchesInspectionFilter(q.Get("delivery"), string(b.Delivery)) &&
			matchesInspectionFilter(q.Get("hold_reason"), inspectionHoldReason(b)) {
			return true
		}
	}
	return false
}

func (s *Service) inspectOccurrences(w http.ResponseWriter, r *http.Request) {
	q, limit, valid := inspectionFilters(r.URL.RawQuery)
	if !valid {
		http.Error(w, "invalid occurrence filters", http.StatusBadRequest)
		return
	}
	store := s.durable.store
	token := q.Get("cursor")
	q.Del("cursor")
	q.Del("limit")
	firing := q.Get("lifecycle") == string(LifecycleFiring)
	scope := tupleID(store.prefix, strconv.Itoa(store.cfg.Valkey.Database), q.Encode())
	cursor := inspectionCursor{Scope: scope}
	if token != "" {
		data, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Scope != scope ||
			(cursor.Next == 0 && len(cursor.Pending) == 0) ||
			slices.ContainsFunc(cursor.Pending, func(id string) bool { return !inspectionOccurrenceID.MatchString(id) }) {
			http.Error(w, "invalid occurrence cursor", http.StatusBadRequest)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Durable.Scheduler.OperationTimeout)
	defer cancel()
	page, err := store.inspectPage(ctx, q, firing, limit, cursor, token == "")
	if err != nil {
		http.Error(w, "occurrence state unavailable", http.StatusServiceUnavailable)
		return
	}
	writeInspectionJSON(w, page)
}

func (s *DurableStore) inspectPage(ctx context.Context, q url.Values, firing bool, limit int, cursor inspectionCursor, initial bool) (inspectionPage, error) {
	page := inspectionPage{Items: make([]occurrenceListItem, 0, limit)}
	if err := s.checkSchema(ctx, s.client); err != nil {
		return page, err
	}
	candidates, scans := 0, 0
	for len(page.Items) < limit && candidates < inspectionCandidateBudget {
		if len(cursor.Pending) == 0 {
			if (!initial && cursor.Next == 0) || scans == inspectionScanBudget {
				break
			}
			ids, next, err := s.scanInspection(ctx, firing, cursor.Next)
			if err != nil {
				return page, err
			}
			cursor.Next, cursor.Pending = next, ids
			scans++
			initial = false
			continue
		}
		id := cursor.Pending[0]
		cursor.Pending = cursor.Pending[1:]
		candidates++
		row, err := s.load(ctx, id)
		if err != nil {
			return page, err
		}
		if row == nil {
			continue
		}
		view := inspectOccurrenceView(row)
		if !matchesInspection(q, view, row) {
			continue
		}
		item := occurrenceListItem{occurrenceView: view, Bindings: make([]occurrenceBindingView, 0, len(row.Bindings))}
		for _, b := range orderedInspectionBindings(row) {
			item.Bindings = append(item.Bindings, inspectBinding(b))
		}
		page.Items = append(page.Items, item)
	}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	if cursor.Next != 0 || len(cursor.Pending) != 0 {
		// SCAN's COUNT is a hint. Preserve the unconsumed page across requests.
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
