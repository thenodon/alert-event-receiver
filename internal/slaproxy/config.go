// Package slaproxy implements the independent, observe-only SLA proxy.
package slaproxy

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Keep IANA validation available in minimal deployment images.

	"go.yaml.in/yaml/v3"
)

// Config is independent of the event receiver's environment configuration.
type Config struct {
	ListenAddress         string        `yaml:"listen_address"`
	WebhookPath           string        `yaml:"webhook_path"`
	MaxBodyBytes          int64         `yaml:"max_body_bytes"`
	ShutdownTimeout       time.Duration `yaml:"shutdown_timeout"`
	EvaluationHorizonDays int           `yaml:"evaluation_horizon_days"`
	SLADefinitions        []Definition  `yaml:"sla_definitions"`
}

// Definition owns its policy namespace. SLA names may repeat across definitions.
type Definition struct {
	SLALabel string            `yaml:"sla_label"`
	SLAs     map[string]Policy `yaml:"slas"`
}

// Policy is evaluated independently within its definition's namespace.
type Policy struct {
	Timezone string              `yaml:"timezone"`
	Schedule map[string][]string `yaml:"schedule"`
	Holidays *Holidays           `yaml:"holidays,omitempty"`
}

type Holidays struct {
	Country         string   `yaml:"country,omitempty"`
	Subdivision     string   `yaml:"subdivision,omitempty"`
	AdditionalDates []string `yaml:"additional_dates,omitempty"`
}

// LoadConfig accepts exactly one YAML document and rejects unknown/duplicate keys.
func LoadConfig(r io.Reader) (Config, error) {
	cfg := Config{
		ListenAddress: ":9012", WebhookPath: "/webhook",
		MaxBodyBytes: 1 << 20, ShutdownTimeout: 10 * time.Second, EvaluationHorizonDays: 370,
	}
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("configuration must contain exactly one YAML document")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func (c Config) Validate() error {
	_, port, err := net.SplitHostPort(c.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen_address must be host:port: %w", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("listen_address must have a numeric port from 0 to 65535")
	}
	u, err := url.Parse(c.WebhookPath)
	if err != nil || u.Path != c.WebhookPath || !strings.HasPrefix(c.WebhookPath, "/") ||
		strings.ContainsAny(c.WebhookPath, "{} \t\r\n") || strings.Contains(c.WebhookPath, "//") ||
		strings.Contains(c.WebhookPath, "/./") || strings.Contains(c.WebhookPath, "/../") ||
		strings.HasSuffix(c.WebhookPath, "/.") || strings.HasSuffix(c.WebhookPath, "/..") ||
		strings.HasSuffix(c.WebhookPath, "/") {
		return fmt.Errorf("webhook_path must be an absolute, canonical, exact HTTP path")
	}
	for _, reserved := range []string{"/metrics", "/healthz", "/readyz"} {
		if c.WebhookPath == reserved {
			return fmt.Errorf("webhook_path conflicts with %s", reserved)
		}
	}
	if c.MaxBodyBytes <= 0 || c.MaxBodyBytes > 64<<20 {
		return fmt.Errorf("max_body_bytes must be between 1 and 67108864")
	}
	if c.ShutdownTimeout <= 0 || c.ShutdownTimeout > time.Minute {
		return fmt.Errorf("shutdown_timeout must be greater than zero and at most 1m")
	}
	if len(c.SLADefinitions) == 0 {
		return fmt.Errorf("at least one sla_definitions entry is required")
	}
	if c.EvaluationHorizonDays < 1 || c.EvaluationHorizonDays > 3660 {
		return fmt.Errorf("evaluation_horizon_days must be between 1 and 3660")
	}
	seen := make(map[string]bool)
	for i, def := range c.SLADefinitions {
		if !labelName.MatchString(def.SLALabel) || seen[def.SLALabel] {
			return fmt.Errorf("sla_definitions[%d]: sla_label must be a unique, nonempty label name", i)
		}
		seen[def.SLALabel] = true
		if len(def.SLAs) == 0 {
			return fmt.Errorf("sla_definitions[%d]: at least one SLA is required", i)
		}
		for name, policy := range def.SLAs {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("sla_definitions[%d]: SLA name must not be empty", i)
			}
			if err := validatePolicy(policy); err != nil {
				return fmt.Errorf("SLA (%s, %s): %w", def.SLALabel, name, err)
			}
		}
	}
	return nil
}

func validatePolicy(p Policy) error {
	if p.Timezone == "" || p.Timezone == "Local" {
		return fmt.Errorf("timezone must be an explicit IANA time zone")
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return fmt.Errorf("invalid timezone: %w", err)
	}
	if len(p.Schedule) == 0 {
		return fmt.Errorf("schedule must contain at least one weekday")
	}
	if _, err := compileSchedule(p.Schedule); err != nil {
		return err
	}
	if h := p.Holidays; h != nil {
		if h.Country == "" && (h.Subdivision != "" || len(h.AdditionalDates) == 0) {
			return fmt.Errorf("holidays requires country or additional_dates; subdivision requires country")
		}
		if h.Country != "" && (len(h.Country) != 2 || h.Country[0] < 'A' || h.Country[0] > 'Z' || h.Country[1] < 'A' || h.Country[1] > 'Z') {
			return fmt.Errorf("holiday country must have two uppercase ASCII letters")
		}
		for _, date := range h.AdditionalDates {
			parsed, err := time.Parse("2006-01-02", date)
			if err != nil || parsed.Format("2006-01-02") != date {
				return fmt.Errorf("invalid additional holiday date %q", date)
			}
		}
	}
	return nil
}

func clockMinutes(s string, allowEndOfDay bool) (int, error) {
	if s == "24:00" && allowEndOfDay {
		return 1440, nil
	}
	if len(s) != 5 || s[2] != ':' || s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' || s[3] < '0' || s[3] > '9' || s[4] < '0' || s[4] > '9' {
		return 0, fmt.Errorf("expected HH:MM")
	}
	hour := int(s[0]-'0')*10 + int(s[1]-'0')
	minute := int(s[3]-'0')*10 + int(s[4]-'0')
	if hour > 23 || minute > 59 {
		return 0, fmt.Errorf("invalid clock time")
	}
	return hour*60 + minute, nil
}
