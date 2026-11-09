package alertload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// These wire models intentionally stand alone. They match the receiver's v4
// webhook contract, without importing any receiver implementation packages.
type Alert struct {
	Status      string            `json:"status"`
	Fingerprint string            `json:"fingerprint"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      *time.Time        `json:"endsAt,omitempty"`
	Labels      map[string]string `json:"labels"`
}
type Payload struct {
	Version     string  `json:"version"`
	Receiver    string  `json:"receiver"`
	Status      string  `json:"status"`
	GroupKey    string  `json:"groupKey"`
	ExternalURL string  `json:"externalURL"`
	Alerts      []Alert `json:"alerts"`
}

// PayloadFor generates one immutable occurrence batch. Each phase reuses the
// original batch index so matching transitions have identical IDs and startsAt.
func PayloadFor(c Config, index int, status string) Payload {
	p := Payload{Version: "4", Receiver: "alertload", Status: status, GroupKey: "alertload", ExternalURL: "http://alertload.invalid", Alerts: make([]Alert, c.BatchSize)}
	for i := range p.Alerts {
		occurrence := int64(index)*int64(c.BatchSize) + int64(i)
		encoded, _ := json.Marshal([3]string{c.RunID, strconv.FormatInt(c.Seed, 10), strconv.FormatInt(occurrence, 10)})
		hash := sha256.Sum256(encoded)
		start := c.BaseTime.UTC().Add(time.Duration(occurrence))
		a := Alert{Status: status, Fingerprint: hex.EncodeToString(hash[:16]), StartsAt: start, Labels: map[string]string{"alertname": "AlertLoadTest", "severity": "warning", "load_run": c.RunID, "load_seed": strconv.FormatInt(c.Seed, 10), "load_occurrence": strconv.FormatInt(occurrence, 10)}}
		if status == "resolved" {
			end := start.Add(c.ResolveAfter)
			a.EndsAt = &end
		}
		p.Alerts[i] = a
	}
	return p
}

type phaseSpec struct {
	name, status, outcome string
	eligible, accepted    *bitset
}
type bitset []uint64

func newBitset(n int) bitset    { return make(bitset, (n+63)/64) }
func (b bitset) has(i int) bool { return b[i/64]&(uint64(1)<<uint(i%64)) != 0 }
func (b bitset) set(i int)      { b[i/64] |= uint64(1) << uint(i%64) }
func plan(c Config, opened, closed *bitset) []phaseSpec {
	switch c.Scenario {
	case "firing":
		return []phaseSpec{{"firing", "firing", "opened", nil, nil}}
	case "orphans":
		return []phaseSpec{{"orphans", "resolved", "resolved_without_open_state", nil, nil}}
	case "lifecycle":
		return []phaseSpec{{"firing", "firing", "opened", nil, opened}, {"resolved", "resolved", "closed", opened, closed}}
	default:
		return []phaseSpec{{"firing", "firing", "opened", nil, opened}, {"repeated_firing", "firing", "repeated_firing", opened, nil}, {"resolved", "resolved", "closed", opened, closed}, {"repeated_resolved", "resolved", "repeated_resolved", closed, nil}}
	}
}
