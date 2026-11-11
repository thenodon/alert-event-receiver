package slaproxy

import (
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

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := LoadConfig(strings.NewReader(exampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestConfigDefaultsAndIndependentNamespaces(t *testing.T) {
	c := testConfig(t)
	if c.ListenAddress != ":9012" || c.WebhookPath != "/webhook" || c.ShutdownTimeout != 10*time.Second || c.MaxBodyBytes != 1<<20 {
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
		"bad address":      func(c *Config) { c.ListenAddress = "localhost" },
		"bad port":         func(c *Config) { c.ListenAddress = ":65536" },
		"service port":     func(c *Config) { c.ListenAddress = ":http" },
		"empty path":       func(c *Config) { c.WebhookPath = "" },
		"relative path":    func(c *Config) { c.WebhookPath = "webhook" },
		"query":            func(c *Config) { c.WebhookPath = "/webhook?x=y" },
		"fragment":         func(c *Config) { c.WebhookPath = "/webhook#x" },
		"pattern":          func(c *Config) { c.WebhookPath = "/{any}" },
		"redirect path":    func(c *Config) { c.WebhookPath = "/a/../webhook" },
		"subtree":          func(c *Config) { c.WebhookPath = "/webhook/" },
		"metrics conflict": func(c *Config) { c.WebhookPath = "/metrics" },
		"health conflict":  func(c *Config) { c.WebhookPath = "/healthz" },
		"ready conflict":   func(c *Config) { c.WebhookPath = "/readyz" },
		"body limit":       func(c *Config) { c.MaxBodyBytes = 0 },
		"large body limit": func(c *Config) { c.MaxBodyBytes = 65 << 20 },
		"shutdown":         func(c *Config) { c.ShutdownTimeout = 0 },
		"long shutdown":    func(c *Config) { c.ShutdownTimeout = 2 * time.Minute },
		"no definitions":   func(c *Config) { c.SLADefinitions = nil },
		"empty label":      func(c *Config) { c.SLADefinitions[0].SLALabel = "" },
		"invalid label":    func(c *Config) { c.SLADefinitions[0].SLALabel = "sla id" },
		"duplicate label":  func(c *Config) { c.SLADefinitions[1].SLALabel = "sla_id" },
		"empty slas":       func(c *Config) { c.SLADefinitions[0].SLAs = nil },
		"empty name":       func(c *Config) { c.SLADefinitions[0].SLAs[" "] = c.SLADefinitions[0].SLAs["business"] },
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

func TestHolidaySyntaxWithoutCoverageEvaluation(t *testing.T) {
	for _, tc := range []struct {
		h     *Holidays
		valid bool
	}{
		{nil, true}, {&Holidays{}, false},
		{&Holidays{Country: "SE", Subdivision: "UNVERIFIED"}, true},
		{&Holidays{Country: "ZZ"}, true}, // Coverage deliberately deferred.
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
