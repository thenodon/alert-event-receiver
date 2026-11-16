package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	maxBody := flag.Int64("max-body", 1<<20, "maximum request body in bytes")
	flag.Parse()
	user, pass := os.Getenv("MOCK_USERNAME"), os.Getenv("MOCK_PASSWORD")
	if (user == "") != (pass == "") {
		log.Fatal("set both MOCK_USERNAME and MOCK_PASSWORD, or neither")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/inbound_event", func(w http.ResponseWriter, r *http.Request) {
		if user != "" {
			u, p, ok := r.BasicAuth()
			if !ok || u != user || p != pass {
				w.Header().Set("WWW-Authenticate", `Basic realm="servicenow-mock"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, *maxBody))
		if err != nil {
			http.Error(w, "invalid or oversized body", http.StatusRequestEntityTooLarge)
			return
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
			http.Error(w, "invalid JSON object", http.StatusBadRequest)
			return
		}
		var alerts []json.RawMessage
		if raw, ok := payload["alerts"]; ok {
			_ = json.Unmarshal(raw, &alerts)
		}
		var status string
		_ = json.Unmarshal(payload["status"], &status)
		pretty, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Printf("\n=== Webhook received %s ===\nsource=%q status=%q alerts=%d idempotency_key=%q remote=%q\n%s\n", time.Now().Format(time.RFC3339), r.URL.Query().Get("source"), status, len(alerts), r.Header.Get("Idempotency-Key"), r.RemoteAddr, pretty)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"accepted"}`+"\n")
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("ServiceNow mock listening on %s (basic auth enabled: %t)", *listen, user != "")
	log.Fatal(server.ListenAndServe())
}
