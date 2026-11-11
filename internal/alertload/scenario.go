package alertload

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultMix = "firing=1,lifecycle=1,duplicates=1,orphans=1"

type Workload struct {
	Scenario     string `json:"scenario"`
	Weight       int    `json:"weight"`
	ResolveAfter string `json:"resolve_after"`
	Cohorts      int    `json:"planned_batch_cohorts"`
}

func validProfile(name string) bool {
	switch name {
	case "firing", "lifecycle", "duplicates", "orphans", "stale", "reopen", "supersede":
		return true
	}
	return false
}

// Scenario files specify weighted occurrence cohorts, not arbitrary HTTP payloads
// or percentages of resolutions. Input is bounded and validated before traffic.
func workloads(c Config) ([]Workload, error) {
	var profiles []Workload
	if c.ScenarioFile != "" {
		if c.Scenario != "mixed" {
			return nil, errors.New("scenario-file requires mixed scenario")
		}
		f, err := os.Open(c.ScenarioFile)
		if err != nil {
			return nil, errors.New("cannot open scenario file")
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		if err != nil || len(data) > 1<<20 {
			return nil, errors.New("scenario file cannot exceed 1 MiB")
		}
		switch strings.ToLower(filepath.Ext(c.ScenarioFile)) {
		case ".json":
			var doc struct {
				Profiles []Workload `json:"profiles"`
			}
			d := json.NewDecoder(strings.NewReader(string(data)))
			d.DisallowUnknownFields()
			if d.Decode(&doc) != nil || d.Decode(new(any)) != io.EOF {
				return nil, errors.New("invalid scenario JSON; expected profiles array")
			}
			profiles = doc.Profiles
		case ".csv":
			r := csv.NewReader(strings.NewReader(string(data)))
			header, err := r.Read()
			if err != nil || strings.Join(header, ",") != "scenario,weight,resolve_after" {
				return nil, errors.New("CSV header must be scenario,weight,resolve_after")
			}
			for {
				row, err := r.Read()
				if err == io.EOF {
					break
				}
				if err != nil || len(profiles) >= 7 {
					return nil, errors.New("invalid scenario CSV or too many profiles")
				}
				weight, err := strconv.Atoi(row[1])
				if err != nil {
					return nil, errors.New("invalid CSV weight")
				}
				profiles = append(profiles, Workload{Scenario: row[0], Weight: weight, ResolveAfter: row[2]})
			}
		default:
			return nil, errors.New("scenario file must be .json or .csv")
		}
	} else if c.Scenario == "mixed" {
		for _, part := range strings.Split(c.Mix, ",") {
			name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
			weight, err := strconv.Atoi(value)
			if !ok || err != nil {
				return nil, errors.New("mix must be comma-separated scenario=positive_integer pairs")
			}
			profiles = append(profiles, Workload{Scenario: name, Weight: weight})
		}
	} else {
		profiles = []Workload{{Scenario: c.Scenario, Weight: 1}}
	}
	if len(profiles) == 0 || len(profiles) > 7 {
		return nil, errors.New("specify one to seven profiles")
	}
	seen := map[string]bool{}
	for i := range profiles {
		p := &profiles[i]
		if !validProfile(p.Scenario) || seen[p.Scenario] || p.Weight < 1 || p.Weight > 1_000_000 {
			return nil, errors.New("profiles must have unique supported scenarios and weights 1..1000000")
		}
		seen[p.Scenario] = true
		if p.Cohorts != 0 {
			return nil, errors.New("planned_batch_cohorts is output-only")
		}
		duration := c.ResolveAfter
		if p.ResolveAfter != "" {
			var err error
			duration, err = time.ParseDuration(p.ResolveAfter)
			if err != nil || duration < 0 {
				return nil, errors.New("invalid profile resolve_after")
			}
		}
		p.ResolveAfter = duration.String()
		last := c.BaseTime.UTC().Add(time.Duration(int64(c.RequestsPerPhase()) * int64(c.BatchSize)))
		for range 3 {
			last = last.Add(duration).Add(time.Second)
		}
		if _, err := last.MarshalText(); err != nil {
			return nil, errors.New("profile timestamps exceed RFC3339 range")
		}
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Scenario < profiles[j].Scenario })
	return profiles, nil
}

type stepTemplate struct {
	status, outcome string
	generation      int
}

func profileSteps(name string) []stepTemplate {
	switch name {
	case "firing":
		return []stepTemplate{{"firing", "opened", 0}}
	case "orphans":
		return []stepTemplate{{"resolved", "resolved_without_open_state", 0}}
	case "lifecycle":
		return []stepTemplate{{"firing", "opened", 0}, {"resolved", "closed", 0}}
	case "duplicates":
		return []stepTemplate{{"firing", "opened", 0}, {"firing", "repeated_firing", 0}, {"resolved", "closed", 0}, {"resolved", "repeated_resolved", 0}}
	case "stale":
		return []stepTemplate{{"firing", "opened", 1}, {"firing", "stale_firing", 0}, {"resolved", "stale_resolved", 0}, {"resolved", "closed", 1}}
	case "reopen":
		return []stepTemplate{{"firing", "opened", 0}, {"resolved", "closed", 0}, {"firing", "reopened", 1}, {"resolved", "closed", 1}}
	default:
		return []stepTemplate{{"firing", "opened", 0}, {"firing", "superseded_firing", 1}, {"resolved", "stale_resolved", 0}, {"resolved", "closed", 1}}
	}
}

// buildPlan apportions exact cohort counts by largest remainder, then shuffles
// their assignment using the seed. Each round interleaves profiles by batch ID.
// Successful predecessor bitsets gate every dependent step, including ambiguous
// repeats/stale steps, so failures conservatively reduce the tested workload.
func buildPlan(c Config) ([]phaseSpec, []Workload, error) {
	ps, err := workloads(c)
	if err != nil {
		return nil, nil, err
	}
	n := c.RequestsPerPhase()
	var total int64
	for _, p := range ps {
		total += int64(p.Weight)
	}
	type remainder struct {
		i     int
		value int64
	}
	remainders := make([]remainder, len(ps))
	assigned := 0
	for i := range ps {
		product := int64(n) * int64(ps[i].Weight)
		ps[i].Cohorts = int(product / total)
		assigned += ps[i].Cohorts
		remainders[i] = remainder{i, product % total}
	}
	sort.SliceStable(remainders, func(i, j int) bool { return remainders[i].value > remainders[j].value })
	for i := 0; i < n-assigned; i++ {
		ps[remainders[i].i].Cohorts++
	}
	cohorts := make([]byte, 0, n)
	for i, p := range ps {
		for range p.Cohorts {
			cohorts = append(cohorts, byte(i))
		}
	}
	rand.New(rand.NewSource(c.Seed)).Shuffle(n, func(i, j int) { cohorts[i], cohorts[j] = cohorts[j], cohorts[i] })
	rounds := make([]phaseSpec, 4)
	for i := range rounds {
		rounds[i] = phaseSpec{name: fmt.Sprintf("mixed_%d", i+1), status: "mixed", outcome: "mixed"}
	}
	for profileIndex, p := range ps {
		var previous *bitset
		duration, _ := time.ParseDuration(p.ResolveAfter)
		for round, t := range profileSteps(p.Scenario) {
			accepted := newBitset(n)
			step := phaseSpec{name: p.Scenario, status: t.status, outcome: t.outcome, profile: p.Scenario, generation: t.generation, resolveAfter: duration, customTime: true, eligible: previous, accepted: &accepted, cohorts: cohorts, cohortID: byte(profileIndex)}
			rounds[round].steps = append(rounds[round].steps, step)
			previous = &accepted
		}
	}
	for len(rounds) > 0 && len(rounds[len(rounds)-1].steps) == 0 {
		rounds = rounds[:len(rounds)-1]
	}
	if c.Scenario != "mixed" {
		for i := range rounds {
			step := rounds[i].steps[0]
			rounds[i].name = fmt.Sprintf("%s_%d", c.Scenario, i+1)
			rounds[i].status = step.status
			rounds[i].outcome = step.outcome
		}
	}
	return rounds, ps, nil
}
func (p phaseSpec) at(i int, checkEligibility bool) (phaseSpec, bool) {
	if len(p.steps) > 0 {
		for _, step := range p.steps {
			if selected, ok := step.at(i, checkEligibility); ok {
				return selected, true
			}
		}
		return phaseSpec{}, false
	}
	if p.cohorts != nil && p.cohorts[i] != p.cohortID {
		return phaseSpec{}, false
	}
	if checkEligibility && p.eligible != nil && !(*p.eligible).has(i) {
		return phaseSpec{}, false
	}
	return p, true
}
func payloadForStep(c Config, index int, p phaseSpec) Payload {
	if p.customTime {
		c.ResolveAfter = p.resolveAfter
	}
	payload := PayloadFor(c, index, p.status)
	for i := range payload.Alerts {
		a := &payload.Alerts[i]
		for range p.generation {
			a.StartsAt = a.StartsAt.Add(c.ResolveAfter).Add(time.Second)
		}
		if p.status == "resolved" {
			end := a.StartsAt.Add(c.ResolveAfter)
			a.EndsAt = &end
		}
	}
	return payload
}
