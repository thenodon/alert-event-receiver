// Package alertload implements a black-box Alertmanager webhook load generator.
// It deliberately does not import receiver models, processor, or state packages.
package alertload

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"regexp"
	"time"
)

const MaxPhaseRequests = 1_000_000

// Config controls a run. Each nonempty phase has its own scheduling window.
// The hard request limit bounds eligibility bitsets independently of run length.
type Config struct {
	URL                string        `json:"-"`
	Rate               float64       `json:"target_requests_per_second"`
	Duration           time.Duration `json:"-"`
	BatchSize          int           `json:"batch_size"`
	Workers            int           `json:"workers"`
	Timeout            time.Duration `json:"-"`
	MaxPending         int           `json:"max_pending"`
	Mix                string        `json:"mix,omitempty"`
	ScenarioFile       string        `json:"-"`
	MetricsURL         string        `json:"-"`
	VictoriaLogsURL    string        `json:"-"`
	ComparisonIsolated bool          `json:"comparison_isolated"`
	CheckTimeout       time.Duration `json:"-"`
	SettleTime         time.Duration `json:"-"`
	Scenario           string        `json:"scenario"`
	ResolveAfter       time.Duration `json:"-"`
	Seed               int64         `json:"seed"`
	RunID              string        `json:"run_id"`
	BaseTime           time.Time     `json:"base_time"`
	Output             string        `json:"-"`
	MaxResponseBytes   int64         `json:"max_response_bytes"`
	DrainTimeout       time.Duration `json:"-"`
	AllowRemote        bool          `json:"allow_remote"`
	MaxFailureRate     float64       `json:"max_failure_rate"`
	FailOnDrop         bool          `json:"fail_on_drop"`
}

func DefaultConfig() Config {
	return Config{Mix: defaultMix, CheckTimeout: 5 * time.Second, SettleTime: time.Second, URL: "http://127.0.0.1:9011/webhook", Rate: 10, Duration: time.Minute, BatchSize: 1, Workers: 10, Timeout: 10 * time.Second, MaxPending: 1000, Scenario: "lifecycle", ResolveAfter: 10 * time.Minute, Seed: 1, BaseTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), MaxResponseBytes: 4096, DrainTimeout: 10 * time.Second, FailOnDrop: true}
}

func ParseConfig(args []string, stderr io.Writer) (Config, error) {
	c := DefaultConfig()
	var base string
	fs := flag.NewFlagSet("alertload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.URL, "url", c.URL, "webhook URL (remote hosts require -allow-remote)")
	fs.Float64Var(&c.Rate, "rate", c.Rate, "offered HTTP requests/second per phase, not alerts/second")
	fs.DurationVar(&c.Duration, "duration", c.Duration, "scheduling window per nonempty phase; excludes drain")
	fs.IntVar(&c.BatchSize, "batch-size", c.BatchSize, "alerts per homogeneous webhook batch")
	fs.IntVar(&c.Workers, "workers", c.Workers, "fixed concurrent HTTP workers")
	fs.DurationVar(&c.Timeout, "timeout", c.Timeout, "per-request timeout")
	fs.IntVar(&c.MaxPending, "max-pending", c.MaxPending, "bounded pending request queue")
	fs.StringVar(&c.Mix, "mix", c.Mix, "weighted scenario cohorts for mixed: scenario=weight,...")
	fs.StringVar(&c.ScenarioFile, "scenario-file", "", "JSON/CSV weighted cohort profiles; implies mixed")
	fs.StringVar(&c.MetricsURL, "metrics-url", "", "optional receiver Prometheus exposition URL")
	fs.StringVar(&c.VictoriaLogsURL, "victorialogs-url", "", "optional VictoriaLogs /select/logsql/query URL")
	fs.BoolVar(&c.ComparisonIsolated, "comparison-isolated", false, "confirm dedicated receiver with no other traffic for metric comparison")
	fs.DurationVar(&c.CheckTimeout, "check-timeout", c.CheckTimeout, "timeout for each verification HTTP call")
	fs.DurationVar(&c.SettleTime, "settle-time", c.SettleTime, "bounded delay before final metric scrape/backend query")
	fs.StringVar(&c.Scenario, "scenario", c.Scenario, "firing, lifecycle, duplicates, orphans, mixed, stale, reopen, supersede")
	fs.DurationVar(&c.ResolveAfter, "resolve-after", c.ResolveAfter, "synthetic resolved duration; not a wall-clock delay")
	fs.Int64Var(&c.Seed, "seed", c.Seed, "deterministic occurrence namespace seed")
	fs.StringVar(&c.RunID, "run-id", "", "isolated namespace; generated if omitted")
	fs.StringVar(&base, "base-time", c.BaseTime.Format(time.RFC3339Nano), "deterministic occurrence base timestamp, RFC3339Nano")
	fs.StringVar(&c.Output, "output", "", "optional JSON report path")
	fs.Int64Var(&c.MaxResponseBytes, "max-response-bytes", c.MaxResponseBytes, "maximum response bytes read; bodies are discarded, never reported")
	fs.DurationVar(&c.DrainTimeout, "drain-timeout", c.DrainTimeout, "grace to drain each phase before canceling requests")
	fs.BoolVar(&c.AllowRemote, "allow-remote", false, "explicitly allow non-loopback destinations")
	fs.Float64Var(&c.MaxFailureRate, "max-failure-rate", 0, "maximum failed/sent request fraction (0..1)")
	fs.BoolVar(&c.FailOnDrop, "fail-on-drop", true, "fail run on queue overflow or missed scheduling deadlines")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	explicitScenario, explicitMix := false, false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "scenario" {
			explicitScenario = true
		}
		if f.Name == "mix" {
			explicitMix = true
		}
	})
	if c.ScenarioFile != "" {
		if explicitMix {
			return c, errors.New("scenario-file and explicit mix are mutually exclusive")
		}
		if !explicitScenario {
			c.Scenario = "mixed"
		}
	}
	if explicitMix && c.Scenario != "mixed" {
		return c, errors.New("mix requires mixed scenario")
	}
	if fs.NArg() != 0 {
		return c, errors.New("unexpected positional arguments")
	}
	var err error
	c.BaseTime, err = time.Parse(time.RFC3339Nano, base)
	if err != nil {
		return c, errors.New("invalid -base-time (expected RFC3339Nano)")
	}
	if c.RunID == "" {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return c, errors.New("cannot generate run ID")
		}
		c.RunID = "alertload-" + hex.EncodeToString(b[:])
	}
	return c, c.Validate()
}

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func (c Config) Validate() error {
	if !runIDPattern.MatchString(c.RunID) {
		return errors.New("run ID must be 1..64 ASCII letters/digits/._-, starting with a letter or digit")
	}
	if math.IsNaN(c.Rate) || math.IsInf(c.Rate, 0) || c.Rate <= 0 || c.Rate > 1_000_000 {
		return errors.New("rate must be finite, positive and at most 1000000 requests/second")
	}
	if c.Duration <= 0 || c.Duration > 24*time.Hour {
		return errors.New("duration must be positive and at most 24h")
	}
	if math.Ceil(c.Rate*c.Duration.Seconds()) > MaxPhaseRequests {
		return fmt.Errorf("rate × duration must not exceed %d requests per phase", MaxPhaseRequests)
	}
	if c.BatchSize < 1 || c.BatchSize > 10000 {
		return errors.New("batch-size must be 1..10000")
	}
	if c.Workers < 1 || c.Workers > 1024 || c.Workers*c.BatchSize > 1_000_000 {
		return errors.New("workers must be 1..1024 and workers × batch-size at most 1000000")
	}
	if c.MaxPending < 1 || c.MaxPending > 1_000_000 {
		return errors.New("max-pending must be 1..1000000")
	}
	if c.Timeout <= 0 || c.Timeout > 24*time.Hour || c.DrainTimeout <= 0 || c.DrainTimeout > 24*time.Hour {
		return errors.New("timeout and drain-timeout must be positive and at most 24h")
	}
	if c.ResolveAfter < 0 {
		return errors.New("resolve-after cannot be negative")
	}
	if c.MaxResponseBytes < 0 || c.MaxResponseBytes > 1<<20 {
		return errors.New("max-response-bytes must be 0..1048576")
	}
	if math.IsNaN(c.MaxFailureRate) || math.IsInf(c.MaxFailureRate, 0) || c.MaxFailureRate < 0 || c.MaxFailureRate > 1 {
		return errors.New("max-failure-rate must be finite and in 0..1")
	}
	switch c.Scenario {
	case "firing", "lifecycle", "duplicates", "orphans", "mixed", "stale", "reopen", "supersede":
	default:
		return errors.New("unsupported alertload scenario")
	}
	if c.BaseTime.IsZero() {
		return errors.New("base-time must be nonzero")
	}
	for _, stamp := range []time.Time{c.BaseTime.UTC(), c.BaseTime.UTC().Add(time.Duration(int64(c.RequestsPerPhase()) * int64(c.BatchSize))).Add(c.ResolveAfter)} {
		if _, err := stamp.MarshalText(); err != nil {
			return errors.New("synthetic timestamps exceed RFC3339 range")
		}
	}
	if c.ScenarioFile != "" && c.Mix != defaultMix {
		return errors.New("scenario-file cannot be combined with custom mix")
	}
	if _, err := workloads(c); err != nil {
		return err
	}
	if c.CheckTimeout <= 0 || c.CheckTimeout > time.Minute || c.SettleTime < 0 || c.SettleTime > time.Minute {
		return errors.New("check-timeout must be positive and at most 1m; settle-time must be 0..1m")
	}
	if c.MetricsURL != "" && !c.ComparisonIsolated {
		return errors.New("metrics-url requires comparison-isolated acknowledgement")
	}
	for _, endpoint := range []string{c.URL, c.MetricsURL, c.VictoriaLogsURL} {
		if endpoint != "" {
			if err := validateEndpoint(endpoint, c.AllowRemote); err != nil {
				return err
			}
		}
	}

	return nil
}
func (c Config) RequestsPerPhase() int { return int(math.Ceil(c.Rate * c.Duration.Seconds())) }
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func (c Config) Remote() bool {
	for _, endpoint := range []string{c.URL, c.MetricsURL, c.VictoriaLogsURL} {
		u, err := url.Parse(endpoint)
		if err == nil && u.Host != "" && !isLoopback(u.Hostname()) {
			return true
		}
	}
	return false
}
func (c Config) Endpoint() string {
	u, err := url.Parse(c.URL)
	if err != nil {
		return "invalid endpoint"
	}
	return u.Scheme + "://" + u.Host
} // Never report paths, credentials or queries.

func validateEndpoint(endpoint string, allowRemote bool) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
		return errors.New("destination must be an absolute HTTP(S) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("URL credentials, query strings and fragments are not supported; do not put tokens in URLs")
	}
	if !allowRemote && !isLoopback(u.Hostname()) {
		return errors.New("non-loopback destination requires explicit -allow-remote")
	}
	return nil
}
