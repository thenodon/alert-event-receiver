// Package httpsecurity implements the shared inbound HTTP security contract.
// Configuration is loaded once at startup, independently of application state.
package httpsecurity

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Config is an immutable startup snapshot. Its zero value means HTTP/no auth.
// Credentials are retained only as fixed-size digests; do not log configuration.
type Config struct {
	webhook    credentials
	metrics    credentials
	inspection credentials
	tls        *tls.Config
}

type credentials struct {
	enabled            bool
	username, password [sha256.Size]byte
}

var envName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// LoadFromEnv validates all six shared HTTP_* settings and resolves referenced
// passwords and certificate files before the caller starts any listeners.
func LoadFromEnv() (Config, error) { return load(os.LookupEnv) }

// LoadWithInspectionFromEnv also validates the SLA proxy's inspection credentials.
func LoadWithInspectionFromEnv() (Config, error) {
	c, err := LoadFromEnv()
	if err != nil {
		return Config{}, err
	}
	c.inspection, err = loadCredentials(os.LookupEnv, "HTTP_INSPECTION_BASIC_AUTH_")
	if err != nil {
		return Config{}, err
	}
	if c.inspection.enabled && (c.inspection == c.webhook || c.inspection == c.metrics) {
		return Config{}, fmt.Errorf("inspection credentials must differ from webhook and metrics credentials")
	}
	return c, nil
}

func load(lookup func(string) (string, bool)) (Config, error) {
	var c Config
	var err error
	if c.webhook, err = loadCredentials(lookup, "HTTP_WEBHOOK_BASIC_AUTH_"); err != nil {
		return Config{}, err
	}
	if c.metrics, err = loadCredentials(lookup, "HTTP_METRICS_BASIC_AUTH_"); err != nil {
		return Config{}, err
	}
	cert, _ := lookup("HTTP_TLS_CERT_FILE")
	key, _ := lookup("HTTP_TLS_KEY_FILE")
	if cert == "" && key == "" {
		return c, nil
	}
	if strings.TrimSpace(cert) == "" || strings.TrimSpace(key) == "" {
		return Config{}, fmt.Errorf("HTTP_TLS_CERT_FILE and HTTP_TLS_KEY_FILE must both be nonempty paths")
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return Config{}, fmt.Errorf("HTTP_TLS_CERT_FILE/HTTP_TLS_KEY_FILE: cannot load matching PEM certificate and private key")
	}
	c.tls = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	return c, nil
}

func unsafeText(value string) bool {
	if !utf8.ValidString(value) {
		return true
	}
	return strings.ContainsFunc(value, unicode.IsControl)
}

func loadCredentials(lookup func(string) (string, bool), prefix string) (credentials, error) {
	username, _ := lookup(prefix + "USERNAME")
	reference, _ := lookup(prefix + "PASSWORD_ENV")
	if username == "" && reference == "" {
		return credentials{}, nil
	}
	if strings.TrimSpace(username) == "" || strings.Contains(username, ":") || unsafeText(username) {
		return credentials{}, fmt.Errorf("%sUSERNAME must be a nonempty Basic Auth username without colon or control characters", prefix)
	}
	if !envName.MatchString(reference) {
		return credentials{}, fmt.Errorf("%sPASSWORD_ENV must name a password environment variable", prefix)
	}
	password, exists := lookup(reference)
	if !exists || password == "" || unsafeText(password) {
		return credentials{}, fmt.Errorf("%sPASSWORD_ENV must reference a nonempty password without control characters", prefix)
	}
	return credentials{enabled: true, username: sha256.Sum256([]byte(username)), password: sha256.Sum256([]byte(password))}, nil
}

func (auth credentials) wrap(realm string, next http.Handler) http.Handler {
	if !auth.enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		userHash, passHash := sha256.Sum256([]byte(username)), sha256.Sum256([]byte(password))
		// Evaluate both comparisons, including when one credential is wrong.
		valid := subtle.ConstantTimeCompare(userHash[:], auth.username[:]) & subtle.ConstantTimeCompare(passHash[:], auth.password[:])
		if !ok || valid != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Webhook protects only the handler registered for the application's webhook.
func (c Config) Webhook(next http.Handler) http.Handler { return c.webhook.wrap("webhook", next) }

// Metrics protects only the metrics handler, independently of the webhook.
func (c Config) Metrics(next http.Handler) http.Handler { return c.metrics.wrap("metrics", next) }

func (c Config) Inspection(next http.Handler) http.Handler {
	return c.inspection.wrap("inspection", next)
}

// Scheme reports the listener protocol without exposing configuration material.
func (c Config) Scheme() string {
	if c.tls != nil {
		return "https"
	}
	return "http"
}

// Serve owns listener exactly as http.Server.Serve does. The caller continues to
// own server.Shutdown/Close and its lifecycle. TLS is wrapped exactly once, using
// the startup snapshot, not reread files. Server timeouts/handlers are preserved.
func (c Config) Serve(server *http.Server, listener net.Listener) error {
	if c.tls != nil {
		server.TLSConfig = c.tls.Clone()
		listener = tls.NewListener(listener, server.TLSConfig)
	}
	return server.Serve(listener)
}

// ListenAndServe binds the server's existing address and uses the same serving
// path as Serve. It opens no secondary listener and performs no redirects.
func (c Config) ListenAndServe(server *http.Server) error {
	address := server.Addr
	if address == "" {
		address = ":http"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	return c.Serve(server, listener)
}
