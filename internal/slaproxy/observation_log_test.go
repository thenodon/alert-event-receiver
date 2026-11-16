package slaproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

func TestObservationLogMatchesHTTPResponse(t *testing.T) {
	for _, failure := range []bool{false, true} {
		name := "success"
		if failure {
			name = "evaluation error"
		}
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			s, err := New(testConfig(t), slog.New(slog.NewJSONHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			s.now = func() time.Time { return instant(t, "2026-11-12T08:14:15.937611979Z") }
			if failure {
				s.evaluator = evaluatorFunc(func(context.Context, PolicyInput, PolicyRef, string, time.Time) (Decision, error) {
					return Decision{}, errors.New("calendar unavailable")
				})
			}
			alert := alertAt(t, "firing", "sla_id", "2026-11-09T09:00:00.123456789+01:00")
			alert.Labels["private_context"] = "unlogged-label-value"
			alert.Annotations["private_context"] = "unlogged-annotation-value"
			skipped := alertAt(t, "resolved", "missing_label", "2026-11-09T10:00:00+01:00")
			w := post(s, testPayload(t, alert, skipped))
			wantStatus := 200
			if failure {
				wantStatus = 500
			}
			if w.Code != wantStatus {
				t.Fatal(w.Code, w.Body)
			}
			var response map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(&logs)
			observations := 0
			for {
				var entry map[string]any
				err := decoder.Decode(&entry)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if entry["msg"] != "webhook observation" {
					continue
				}
				observations++
				if entry["level"] != "INFO" || entry["component"] != "sla_webhook" || entry["operation"] != "observe" || entry["http_status"] != float64(wantStatus) {
					t.Fatal("missing structured context", entry)
				}
				if !reflect.DeepEqual(entry["observation"], response) {
					t.Fatalf("logged response differs: %+v; HTTP: %+v", entry["observation"], response)
				}
				encoded, err := json.Marshal(entry)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(encoded, []byte("unlogged-label-value")) || bytes.Contains(encoded, []byte("unlogged-annotation-value")) {
					t.Fatal("full alert data was logged")
				}
			}
			if observations != 1 {
				t.Fatalf("expected one batch observation log, got %d", observations)
			}
		})
	}
}

func TestMalformedWebhookDoesNotLogObservation(t *testing.T) {
	var logs bytes.Buffer
	s, err := New(testConfig(t), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if w := post(s, []byte(`{"alerts":`)); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if bytes.Contains(logs.Bytes(), []byte("webhook observation")) || !bytes.Contains(logs.Bytes(), []byte("webhook rejected")) {
		t.Fatal(logs.String())
	}
}
