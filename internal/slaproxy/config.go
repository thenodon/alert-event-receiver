// Package slaproxy implements independent SLA observation and durable lifecycle processing.
package slaproxy

import (
	"encoding/json"
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
	ListenAddress         string         `yaml:"listen_address"`
	WebhookPath           string         `yaml:"webhook_path"`
	MaxBodyBytes          int64          `yaml:"max_body_bytes"`
	ShutdownTimeout       time.Duration  `yaml:"shutdown_timeout"`
	EvaluationHorizonDays int            `yaml:"evaluation_horizon_days"`
	SLADefinitions        []Definition   `yaml:"sla_definitions"`
	Durable               DurableConfig  `yaml:"durable"`
	Delivery              DeliveryConfig `yaml:"delivery"`
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
		Durable: defaultDurableConfig(), Delivery: defaultDeliveryConfig(),
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
	if err := c.Delivery.Validate(c.Durable); err != nil {
		return err
	}
	if err := c.Durable.Validate(); err != nil {
		return err
	}
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
	if len(c.SLADefinitions) == 0 && !c.Durable.Enabled {
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

// DurableConfig configures persistent lifecycle state, scheduling and retention.
type DurableConfig struct {
	Enabled     bool   `yaml:"enabled"`
	PolicyEpoch uint64 `yaml:"policy_epoch"`
	Valkey      struct {
		Address     string `yaml:"address"`
		Username    string `yaml:"username"`
		PasswordEnv string `yaml:"password_env"`
		Database    int    `yaml:"database"`
		Namespace   string `yaml:"namespace"`
		TLS         bool   `yaml:"tls"`
	} `yaml:"valkey"`
	Scheduler struct {
		PollInterval      time.Duration `yaml:"poll_interval"`
		Workers           int           `yaml:"workers"`
		BatchSize         int           `yaml:"batch_size"`
		LeaseDuration     time.Duration `yaml:"lease_duration"`
		OperationTimeout  time.Duration `yaml:"operation_timeout"`
		RetryInitial      time.Duration `yaml:"retry_initial"`
		RetryMax          time.Duration `yaml:"retry_max"`
		ReconcileInterval time.Duration `yaml:"reconcile_interval"`
	} `yaml:"scheduler"`
	Retention struct {
		CompletedPayload time.Duration `yaml:"completed_payload"`
		Tombstone        time.Duration `yaml:"tombstone"`
	} `yaml:"retention"`
}

func defaultDurableConfig() DurableConfig {
	var c DurableConfig
	c.PolicyEpoch = 1
	c.Valkey.Address, c.Valkey.Namespace = "localhost:6379", "sla"
	c.Scheduler.PollInterval, c.Scheduler.Workers, c.Scheduler.BatchSize = time.Second, 4, 100
	c.Scheduler.LeaseDuration, c.Scheduler.OperationTimeout = 30*time.Second, 5*time.Second
	c.Scheduler.RetryInitial, c.Scheduler.RetryMax, c.Scheduler.ReconcileInterval = 5*time.Second, 5*time.Minute, 30*time.Second
	c.Retention.CompletedPayload, c.Retention.Tombstone = 30*24*time.Hour, 90*24*time.Hour
	return c
}

func (c DurableConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.PolicyEpoch == 0 {
		return fmt.Errorf("durable requires positive policy_epoch")
	}
	_, port, err := net.SplitHostPort(c.Valkey.Address)
	if err != nil {
		return fmt.Errorf("durable.valkey.address: %w", err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("durable.valkey.address requires a numeric port from 1 through 65535")
	}
	if c.Valkey.Database < 0 || c.Valkey.Namespace == "" || len(c.Valkey.Namespace) > 64 || strings.ContainsAny(c.Valkey.Namespace, "{}*?[]:\\ \t\r\n") {
		return fmt.Errorf("invalid durable Valkey database/namespace")
	}
	s := c.Scheduler
	if s.PollInterval < 10*time.Millisecond || s.PollInterval > time.Minute || s.Workers < 1 || s.Workers > 64 || s.BatchSize < 1 || s.BatchSize > 1000 {
		return fmt.Errorf("invalid durable scheduler poll_interval/workers/batch_size")
	}
	if s.OperationTimeout <= 0 || s.OperationTimeout > time.Minute || s.LeaseDuration < 2*s.OperationTimeout || s.LeaseDuration > 10*time.Minute {
		return fmt.Errorf("durable lease_duration must be >= 2*operation_timeout and <= 10m")
	}
	if s.RetryInitial < s.PollInterval || s.RetryMax < s.RetryInitial || s.RetryMax > time.Hour || s.ReconcileInterval < s.PollInterval || s.ReconcileInterval > time.Hour {
		return fmt.Errorf("invalid durable scheduler backoff/reconciliation durations")
	}
	if c.Retention.CompletedPayload <= 0 || c.Retention.Tombstone <= c.Retention.CompletedPayload || c.Retention.Tombstone > 10*365*24*time.Hour {
		return fmt.Errorf("durable retention requires 0 < completed_payload < tombstone <= 10 years")
	}
	return nil
}

// DeliveryConfig names independent destinations and enables durable dispatch.
type DeliveryConfig struct {
	Enabled    bool         `yaml:"enabled"`
	Sinks      []SinkConfig `yaml:"sinks"`
	Dispatcher struct {
		PollInterval    time.Duration `yaml:"poll_interval"`
		Workers         int           `yaml:"workers"`
		BatchSize       int           `yaml:"batch_size"`
		ClaimLease      time.Duration `yaml:"claim_lease"`
		DeliveryTimeout time.Duration `yaml:"delivery_timeout"`
		RetryInitial    time.Duration `yaml:"retry_initial"`
		RetryMax        time.Duration `yaml:"retry_max"`
	} `yaml:"dispatcher"`
}

func defaultDeliveryConfig() DeliveryConfig {
	var c DeliveryConfig
	c.Sinks = []SinkConfig{{ID: "default", Type: "log"}}
	c.Dispatcher.PollInterval = time.Second
	c.Dispatcher.Workers, c.Dispatcher.BatchSize = 4, 100
	c.Dispatcher.ClaimLease, c.Dispatcher.DeliveryTimeout = 30*time.Second, 10*time.Second
	c.Dispatcher.RetryInitial, c.Dispatcher.RetryMax = 5*time.Second, 5*time.Minute
	return c
}
func (c DeliveryConfig) Validate(d DurableConfig) error {
	if (d.Enabled || c.Enabled) && (len(c.Sinks) == 0 || len(c.Sinks) > 32) {
		return fmt.Errorf("delivery.sinks requires 1 through 32 named sinks")
	}
	seen := make(map[string]bool)
	for _, sink := range c.Sinks {
		if !sinkIDPattern.MatchString(sink.ID) || seen[sink.ID] {
			return fmt.Errorf("sink IDs must be unique, nonempty ASCII identifiers (max 128 bytes)")
		}
		seen[sink.ID] = true
		if err := sink.Validate(c.Dispatcher.DeliveryTimeout); err != nil {
			return fmt.Errorf("sink %s: %w", sink.ID, err)
		}
	}
	if !c.Enabled {
		return nil
	}
	if !d.Enabled {
		return fmt.Errorf("delivery requires durable.enabled")
	}
	x := c.Dispatcher
	if x.PollInterval < 10*time.Millisecond || x.PollInterval > time.Minute || x.Workers < 1 || x.Workers > 64 || x.BatchSize < 1 || x.BatchSize > 1000 {
		return fmt.Errorf("invalid delivery dispatcher poll_interval/workers/batch_size")
	}
	if x.DeliveryTimeout <= 0 || x.DeliveryTimeout > time.Minute || x.ClaimLease < x.DeliveryTimeout+3*d.Scheduler.OperationTimeout || x.ClaimLease > 10*time.Minute {
		return fmt.Errorf("delivery claim_lease must cover delivery_timeout + 3*durable scheduler operation_timeout and be <= 10m")
	}
	if x.RetryInitial < x.PollInterval || x.RetryMax < x.RetryInitial || x.RetryMax > time.Hour {
		return fmt.Errorf("invalid delivery retry durations")
	}
	return nil
}

// SinkConfig names a stable destination. Fingerprints intentionally exclude
// credential values, allowing secret rotation without redirecting a destination.
type SinkConfig struct {
	ID   string          `yaml:"id"`
	Type string          `yaml:"type"`
	HTTP *HTTPSinkConfig `yaml:"http,omitempty"`
}
type HTTPSinkConfig struct {
	URL       string        `yaml:"url"`
	Timeout   time.Duration `yaml:"timeout"`
	BasicAuth struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"basic_auth"`
	IdempotencyKey *bool `yaml:"idempotency_key,omitempty"`
}

var sinkIDPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func (c SinkConfig) Validate(maximum time.Duration) error {
	switch c.Type {
	case "log":
		if c.HTTP != nil {
			return fmt.Errorf("log sink must not have HTTP settings")
		}
	case "http":
		if c.HTTP == nil {
			return fmt.Errorf("http settings are required")
		}
		u, err := url.Parse(c.HTTP.URL)
		if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(c.HTTP.URL, " \t\r\n") || strings.HasSuffix(u.Host, ":") {
			return fmt.Errorf("invalid HTTP URL: require http/https, host, no userinfo or fragment")
		}
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("invalid HTTP URL port")
			}
		}
		if c.HTTP.Timeout <= 0 || c.HTTP.Timeout > maximum {
			return fmt.Errorf("HTTP timeout must be positive and <= dispatcher.delivery_timeout")
		}
		auth := c.HTTP.BasicAuth
		if (auth.Username == "") != (auth.Password == "") {
			return fmt.Errorf("basic_auth requires both username and password")
		}
		for _, value := range []string{auth.Username, auth.Password} {
			if _, _, err := credentialReference(value); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported sink type; use log or http")
	}
	return nil
}
func (c SinkConfig) fingerprint() string {
	// Fingerprint destination settings and credential references, never credential
	// values. Literal secrets can rotate without redirecting queued deliveries.
	if c.HTTP != nil {
		httpConfig := *c.HTTP
		httpConfig.BasicAuth.Username = credentialIdentity(httpConfig.BasicAuth.Username)
		httpConfig.BasicAuth.Password = credentialIdentity(httpConfig.BasicAuth.Password)
		c.HTTP = &httpConfig
	}
	encoded, _ := json.Marshal(c)
	return tupleID(string(encoded))
}
func (c Config) sinkCatalog() map[string]string {
	catalog := make(map[string]string, len(c.Delivery.Sinks))
	for _, sink := range c.Delivery.Sinks {
		catalog[sink.ID] = sink.fingerprint()
	}
	return catalog
}
func (c Config) sinkConfig(id string) (SinkConfig, bool) {
	for _, sink := range c.Delivery.Sinks {
		if sink.ID == id {
			return sink, true
		}
	}
	return SinkConfig{}, false
}
