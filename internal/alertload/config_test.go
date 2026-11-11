package alertload

import (
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*Config)
	}{
		{"zero rate", func(c *Config) { c.Rate = 0 }}, {"negative rate", func(c *Config) { c.Rate = -1 }}, {"nan", func(c *Config) { c.Rate = math.NaN() }}, {"infinity", func(c *Config) { c.Rate = math.Inf(1) }},
		{"duration", func(c *Config) { c.Duration = 0 }}, {"unbounded plan", func(c *Config) { c.Rate = 1e6; c.Duration = time.Minute }},
		{"batch", func(c *Config) { c.BatchSize = 0 }}, {"workers", func(c *Config) { c.Workers = 0 }}, {"queue", func(c *Config) { c.MaxPending = 0 }},
		{"timeout", func(c *Config) { c.Timeout = 0 }}, {"drain", func(c *Config) { c.DrainTimeout = 0 }}, {"negative duration", func(c *Config) { c.ResolveAfter = -1 }},
		{"unsafe ID", func(c *Config) { c.RunID = "../../escape" }}, {"long ID", func(c *Config) { c.RunID = strings.Repeat("a", 65) }},
		{"unknown profile", func(c *Config) { c.Scenario = "unknown" }}, {"remote", func(c *Config) { c.URL = "https://example.com/webhook" }},
		{"userinfo", func(c *Config) { c.URL = "http://user:private-token@localhost/webhook" }}, {"query", func(c *Config) { c.URL = "http://localhost/webhook?token=private-token" }},
		{"scheme", func(c *Config) { c.URL = "file:///tmp/webhook" }}, {"body cap", func(c *Config) { c.MaxResponseBytes = 2 << 20 }},
		{"failure rate", func(c *Config) { c.MaxFailureRate = 1.1 }}, {"failure nan", func(c *Config) { c.MaxFailureRate = math.NaN() }},
		{"zero base", func(c *Config) { c.BaseTime = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig()
			tc.modify(&c)
			err := c.Validate()
			if err == nil {
				t.Fatal("accepted invalid config")
			}
			if strings.Contains(err.Error(), "private-token") {
				t.Fatal("credential leaked")
			}
		})
	}
	c := testConfig()
	c.ResolveAfter = 0
	if err := c.Validate(); err != nil {
		t.Fatal("zero synthetic duration must be valid", err)
	}
	for _, endpoint := range []string{"http://localhost:9011/webhook", "http://127.0.0.2/webhook", "http://[::1]/webhook"} {
		c.URL = endpoint
		if err := c.Validate(); err != nil || c.Remote() {
			t.Fatalf("loopback rejected: %s %v", endpoint, err)
		}
	}
	c.URL = "https://example.com/webhook"
	c.AllowRemote = true
	if err := c.Validate(); err != nil || !c.Remote() {
		t.Fatal("explicit remote rejected")
	}
}
func TestParseConfigReproducibility(t *testing.T) {
	args := []string{"-run-id", "fixed", "-seed", "42", "-base-time", "2026-11-09T10:00:00.123456789Z", "-resolve-after", "0s"}
	a, err := ParseConfig(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseConfig(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("same flags changed config")
	}
	a, err = ParseConfig(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b, err = ParseConfig(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if a.RunID == b.RunID || !runIDPattern.MatchString(a.RunID) {
		t.Fatal("generated run IDs not unique/safe")
	}
}
