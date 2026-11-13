package slaproxy

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCheckedInConfigurationExample(t *testing.T) {
	f, err := os.Open("../../docs/examples/alert-sla-proxy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := LoadConfig(f); err != nil {
		t.Fatal(err)
	}
}

const exampleConfig = `sla_definitions:
  - sla_label: sla_id
    slas:
      business:
        timezone: Europe/Stockholm
        schedule:
          monday: ["08:00-12:00", "13:00-17:00"]
        holidays:
          country: SE
          additional_dates: ["2027-12-24"]
  - sla_label: sla_name
    slas:
      business:
        timezone: UTC
        schedule:
          monday: ["00:00-24:00"]
`

func testConfig(t testing.TB) Config {
	t.Helper()
	cfg, err := LoadConfig(strings.NewReader(exampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestConfigDefaultsAndIndependentNamespaces(t *testing.T) {
	c := testConfig(t)
	if c.ListenAddress != ":9012" || c.WebhookPath != "/webhook" || c.ShutdownTimeout != 10*time.Second || c.MaxBodyBytes != 1<<20 || c.EvaluationHorizonDays != 370 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.SLADefinitions[0].SLAs["business"].Timezone != "Europe/Stockholm" || c.SLADefinitions[1].SLAs["business"].Timezone != "UTC" {
		t.Fatal("policy namespaces were merged")
	}
	cfg, err := LoadConfig(strings.NewReader("listen_address: 127.0.0.1:9020\nwebhook_path: /alerts\nshutdown_timeout: 2s\nmax_body_bytes: 4096\n" + exampleConfig))
	if err != nil || cfg.ListenAddress != "127.0.0.1:9020" || cfg.WebhookPath != "/alerts" || cfg.ShutdownTimeout != 2*time.Second || cfg.MaxBodyBytes != 4096 {
		t.Fatalf("overrides: %+v, %v", cfg, err)
	}
}

func TestStrictYAML(t *testing.T) {
	for name, input := range map[string]string{
		"empty": "", "null": "null", "malformed": "[",
		"unknown root":           "listen_adress: :9012\n" + exampleConfig,
		"unknown definition":     strings.Replace(exampleConfig, "- sla_label: sla_id", "- sla_label: sla_id\n    extra: true", 1),
		"unknown policy":         strings.Replace(exampleConfig, "timezone: UTC", "timezone: UTC\n        extra: true", 1),
		"unknown holidays":       strings.Replace(exampleConfig, "country: SE", "country: SE\n          exclude: true", 1),
		"duplicate map key":      strings.Replace(exampleConfig, "timezone: UTC", "timezone: UTC\n        timezone: Europe/Berlin", 1),
		"multiple documents":     exampleConfig + "\n---\n{}",
		"trailing null document": exampleConfig + "\n---\nnull",
		"invalid duration":       "shutdown_timeout: later\n" + exampleConfig,
		"wrong type":             "max_body_bytes: many\n" + exampleConfig,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(strings.NewReader(input)); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"bad address":       func(c *Config) { c.ListenAddress = "localhost" },
		"bad port":          func(c *Config) { c.ListenAddress = ":65536" },
		"service port":      func(c *Config) { c.ListenAddress = ":http" },
		"empty path":        func(c *Config) { c.WebhookPath = "" },
		"relative path":     func(c *Config) { c.WebhookPath = "webhook" },
		"query":             func(c *Config) { c.WebhookPath = "/webhook?x=y" },
		"fragment":          func(c *Config) { c.WebhookPath = "/webhook#x" },
		"pattern":           func(c *Config) { c.WebhookPath = "/{any}" },
		"redirect path":     func(c *Config) { c.WebhookPath = "/a/../webhook" },
		"subtree":           func(c *Config) { c.WebhookPath = "/webhook/" },
		"metrics conflict":  func(c *Config) { c.WebhookPath = "/metrics" },
		"health conflict":   func(c *Config) { c.WebhookPath = "/healthz" },
		"ready conflict":    func(c *Config) { c.WebhookPath = "/readyz" },
		"body limit":        func(c *Config) { c.MaxBodyBytes = 0 },
		"large body limit":  func(c *Config) { c.MaxBodyBytes = 65 << 20 },
		"shutdown":          func(c *Config) { c.ShutdownTimeout = 0 },
		"long shutdown":     func(c *Config) { c.ShutdownTimeout = 2 * time.Minute },
		"no definitions":    func(c *Config) { c.SLADefinitions = nil },
		"zero horizon":      func(c *Config) { c.EvaluationHorizonDays = 0 },
		"negative horizon":  func(c *Config) { c.EvaluationHorizonDays = -1 },
		"excessive horizon": func(c *Config) { c.EvaluationHorizonDays = 3661 },
		"empty label":       func(c *Config) { c.SLADefinitions[0].SLALabel = "" },
		"invalid label":     func(c *Config) { c.SLADefinitions[0].SLALabel = "sla id" },
		"duplicate label":   func(c *Config) { c.SLADefinitions[1].SLALabel = "sla_id" },
		"empty slas":        func(c *Config) { c.SLADefinitions[0].SLAs = nil },
		"empty name":        func(c *Config) { c.SLADefinitions[0].SLAs[" "] = c.SLADefinitions[0].SLAs["business"] },
	} {
		t.Run(name, func(t *testing.T) {
			c := testConfig(t)
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestPolicySyntax(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy Policy
		valid  bool
	}{
		{"split day", Policy{Timezone: "UTC", Schedule: map[string][]string{"monday": {"08:00-12:00", "13:00-17:00"}}}, true},
		{"adjacent", Policy{Timezone: "UTC", Schedule: map[string][]string{"monday": {"12:00-17:00", "08:00-12:00"}}}, true},
		{"all day", Policy{Timezone: "UTC", Schedule: map[string][]string{"sunday": {"00:00-24:00"}}}, true},
		{"overnight syntax", Policy{Timezone: "UTC", Schedule: map[string][]string{"friday": {"22:00-06:00", "08:00-10:00"}}}, true},
		{"overnight overlap", Policy{Timezone: "UTC", Schedule: map[string][]string{"friday": {"22:00-06:00", "23:00-02:00"}}}, false},
		{"overlap", Policy{Timezone: "UTC", Schedule: map[string][]string{"monday": {"10:00-14:00", "08:00-12:00"}}}, false},
		{"duplicate window", Policy{Timezone: "UTC", Schedule: map[string][]string{"monday": {"08:00-12:00", "08:00-12:00"}}}, false},
		{"bad weekday", Policy{Timezone: "UTC", Schedule: map[string][]string{"Monday": {"08:00-17:00"}}}, false},
		{"bad zone", Policy{Timezone: "Mars/Olympus", Schedule: map[string][]string{"monday": {}}}, false},
		{"local zone", Policy{Timezone: "Local", Schedule: map[string][]string{"monday": {}}}, false},
		{"no zone", Policy{Schedule: map[string][]string{"monday": {}}}, false},
		{"no schedule", Policy{Timezone: "UTC"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validatePolicy(tc.policy); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
	for _, window := range []string{"8:00-17:00", "08:60-17:00", "24:00-01:00", "08:00-25:00", "08:00-08:00", "08:00", "08:00-17:00-extra", "aa:bb-17:00", "08:00 -17:00"} {
		t.Run(window, func(t *testing.T) {
			p := Policy{Timezone: "UTC", Schedule: map[string][]string{"monday": {window}}}
			if err := validatePolicy(p); err == nil {
				t.Fatal("invalid window accepted")
			}
		})
	}
}

func TestHolidayBlockSyntaxBeforeProviderValidation(t *testing.T) {
	for _, tc := range []struct {
		h     *Holidays
		valid bool
	}{
		{nil, true}, {&Holidays{}, false},
		{&Holidays{Country: "SE", Subdivision: "UNVERIFIED"}, true},
		{&Holidays{Country: "ZZ"}, true}, // Coverage is validated by the engine at startup.
		{&Holidays{Country: "se"}, false},
		{&Holidays{Country: "SWE"}, false},
		{&Holidays{Subdivision: "BY"}, false},
		{&Holidays{AdditionalDates: []string{"2028-02-29", "2027-12-24"}}, true},
		{&Holidays{AdditionalDates: []string{"2027-02-29"}}, false},
		{&Holidays{AdditionalDates: []string{"2027-1-01"}}, false},
		{&Holidays{AdditionalDates: []string{""}}, false},
	} {
		p := Policy{Timezone: "UTC", Schedule: map[string][]string{"monday": {}}, Holidays: tc.h}
		if err := validatePolicy(p); (err == nil) != tc.valid {
			t.Fatalf("holidays %+v, valid=%v: %v", tc.h, tc.valid, err)
		}
	}
}

func TestDurableConfigExample(t *testing.T) {
	f, err := os.Open("../../docs/examples/alert-sla-proxy-durable.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := LoadConfig(f)
	if err != nil || !c.Durable.Enabled {
		t.Fatal(c, err)
	}
}

func TestDurableDefaultsAndValidation(t *testing.T) {
	c := testConfig(t)
	if c.Durable.Enabled {
		t.Fatal("legacy config enables persistence")
	}
	c.Durable.Enabled = true
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	drain := c
	drain.SLADefinitions = nil
	if err := drain.Validate(); err != nil {
		t.Fatal("durable resolution-only catalog rejected", err)
	}
	if c.Durable.Retention.CompletedPayload != 30*24*time.Hour || c.Durable.Retention.Tombstone != 90*24*time.Hour {
		t.Fatal(c.Durable.Retention)
	}
	for name, mutate := range map[string]func(*DurableConfig){
		"epoch":     func(c *DurableConfig) { c.PolicyEpoch = 0 },
		"sink":      func(c *DurableConfig) { c.SinkID = "" },
		"address":   func(c *DurableConfig) { c.Valkey.Address = "invalid" },
		"namespace": func(c *DurableConfig) { c.Valkey.Namespace = "bad:*" },
		"database":  func(c *DurableConfig) { c.Valkey.Database = -1 },
		"workers":   func(c *DurableConfig) { c.Scheduler.Workers = 0 },
		"batch":     func(c *DurableConfig) { c.Scheduler.BatchSize = 1001 },
		"poll":      func(c *DurableConfig) { c.Scheduler.PollInterval = time.Millisecond },
		"lease":     func(c *DurableConfig) { c.Scheduler.LeaseDuration = c.Scheduler.OperationTimeout },
		"retry":     func(c *DurableConfig) { c.Scheduler.RetryMax = time.Millisecond },
		"reconcile": func(c *DurableConfig) { c.Scheduler.ReconcileInterval = time.Millisecond },
		"retention": func(c *DurableConfig) { c.Retention.Tombstone = c.Retention.CompletedPayload },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := c
			mutate(&invalid.Durable)
			if err := invalid.Validate(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	for _, fragment := range []string{"  freshness: {mode: api}\n", "  valkey: {script: true}\n", "  unknown: true\n"} {
		if _, err := LoadConfig(strings.NewReader(exampleConfig + "durable:\n  enabled: true\n" + fragment)); err == nil {
			t.Fatal("unused/verification option accepted", fragment)
		}
	}
}

func TestDurableDimensionsAndLateResolution(t *testing.T) {
	cfg := testConfig(t)
	m := &DurableManager{cfg: cfg, engine: engineFor(t, businessPolicy(), 370)}
	start := instant(t, "2026-11-09T07:30:00+01:00")
	first := instant(t, "2026-11-09T08:00:00+01:00")
	b := &DurableBinding{ID: "binding", SinkID: cfg.Durable.SinkID, Policy: PolicyRef{"sla_id", "business"}, PolicyVersion: m.engine.policies[PolicyRef{"sla_id", "business"}].version, Calendar: Decision{NextEvaluationAt: first}, Intents: map[string]*DeliveryIntent{}, Delivery: NotDelivered}
	row := &DurableOccurrence{ID: "occurrence", StartsAt: start, Lifecycle: LifecycleFiring, Bindings: map[string]*DurableBinding{b.ID: b}}
	m.advance(row, start.Add(time.Minute))
	if b.Eligibility != EligibilityPending || row.Lifecycle != LifecycleFiring || b.Delivery != NotDelivered {
		t.Fatal(row, b)
	}
	m.advance(row, first)
	if b.Eligibility != EligibilityEligible || b.Delivery != PendingDelivery || !b.Intents["firing"].Ready || row.LastVerifiedActiveAt != nil {
		t.Fatal(b)
	}
	row.Lifecycle, row.EndsAt = LifecycleResolved, utcPointer(first.Add(-time.Minute))
	m.advance(row, first.Add(time.Hour))
	if b.Eligibility != EligibilityIneligible || b.Intents["firing"].Ready || !b.Intents["firing"].Canceled || b.Delivery == Delivered {
		t.Fatal(b)
	}
	// Reserved Phase 4 receipt evidence must preserve resolution continuity
	// even if a delayed resolution corrects earlier provisional eligibility.
	b.FiringDeliveredAt = utcPointer(first)
	m.advance(row, first.Add(2*time.Hour))
	if !b.Intents["resolved"].Ready || b.Delivery != PendingDelivery {
		t.Fatal(b)
	}
}

func TestDurableSLAOpportunities(t *testing.T) {
	for _, tc := range []struct {
		name, zone, day, window, start string
		holidays                       *Holidays
	}{
		{"overnight", "Europe/Stockholm", "monday", "22:00-06:00", "2026-11-09T21:30:00+01:00", nil},
		{"holiday", "Europe/Stockholm", "friday", "08:00-17:00", "2027-01-01T07:30:00+01:00", &Holidays{Country: "SE"}},
		{"spring", "Europe/Stockholm", "sunday", "02:30-03:30", "2026-03-29T00:30:00Z", nil},
		{"autumn", "Europe/Stockholm", "sunday", "02:15-02:45", "2026-10-25T00:50:00Z", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			p := Policy{Timezone: tc.zone, Schedule: map[string][]string{tc.day: {tc.window}}, Holidays: tc.holidays}
			e := engineFor(t, p, 370)
			m := &DurableManager{cfg: cfg, engine: e}
			start := instant(t, tc.start)
			ref := PolicyRef{"sla_id", "business"}
			d, err := e.Evaluate(context.Background(), PolicyInput{}, ref, "", start)
			if err != nil || d.NextEvaluationAt.IsZero() {
				t.Fatal(d, err)
			}
			b := &DurableBinding{ID: "binding", Policy: ref, SinkID: cfg.Durable.SinkID, PolicyVersion: d.PolicyVersion, Calendar: d, Intents: map[string]*DeliveryIntent{}}
			row := &DurableOccurrence{ID: "occurrence", StartsAt: start, Lifecycle: LifecycleFiring, Bindings: map[string]*DurableBinding{b.ID: b}}
			m.advance(row, start)
			if b.Eligibility != EligibilityPending || !b.NextEvaluationAt.Equal(d.NextEvaluationAt) {
				t.Fatal(b)
			}
			m.advance(row, d.NextEvaluationAt)
			if b.Eligibility != EligibilityEligible || b.Delivery != PendingDelivery || !b.Intents["firing"].Ready {
				t.Fatal(b)
			}
		})
	}
}

func TestSchedulerBackoffBounded(t *testing.T) {
	m := &DurableManager{cfg: testConfig(t)}
	for _, attempt := range []int{0, 1, 10, 100000} {
		if d := m.retryDelay(attempt); d < m.cfg.Durable.Scheduler.RetryInitial || d > m.cfg.Durable.Scheduler.RetryMax {
			t.Fatal(d)
		}
	}
}

func TestRejectStageNamedConfigurationKey(t *testing.T) {
	if _, err := LoadConfig(strings.NewReader(exampleConfig + "phase3:\n  enabled: true\n")); err == nil {
		t.Fatal("obsolete phase3 YAML key accepted")
	}
}
