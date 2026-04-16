package telemetry

import "testing"

func TestParseHeaderMap_SimplePairs(t *testing.T) {
	h, err := parseHeaderMap("Authorization=Bearer token,X-Scope-OrgID=1")
	if err != nil {
		t.Fatal(err)
	}
	if h["Authorization"] != "Bearer token" {
		t.Fatalf("unexpected Authorization: %q", h["Authorization"])
	}
	if h["X-Scope-OrgID"] != "1" {
		t.Fatalf("unexpected X-Scope-OrgID: %q", h["X-Scope-OrgID"])
	}
}

func TestParseHeaderMap_CommaValueContinuation(t *testing.T) {
	raw := "VL-Stream-Fields=alertname,fingerprint,fingerprint_source,severity,state_write_result,status,transition"
	h, err := parseHeaderMap(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := "alertname,fingerprint,fingerprint_source,severity,state_write_result,status,transition"
	if h["VL-Stream-Fields"] != want {
		t.Fatalf("unexpected VL-Stream-Fields: got %q want %q", h["VL-Stream-Fields"], want)
	}
}

func TestParseHeaderMap_InvalidFirstSegment(t *testing.T) {
	_, err := parseHeaderMap("fingerprint")
	if err == nil {
		t.Fatal("expected error")
	}
}

