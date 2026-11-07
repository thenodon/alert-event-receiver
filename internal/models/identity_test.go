package models

import (
	"encoding/hex"
	"testing"
	"time"
)

func TestLifecycleEventID(t *testing.T) {
	start := time.Date(2026, 11, 6, 11, 0, 0, 123456789, time.UTC)
	id := LifecycleEventID("abc123", start, "firing")
	if id != "cd2f23a114c62ccf0c83bcee8a6a104e60c4483ff09bf17898fa114315586a70" {
		t.Fatalf("identity encoding changed: %s", id)
	}
	if _, err := hex.DecodeString(id); err != nil || len(id) != 64 {
		t.Fatalf("invalid digest: %s", id)
	}
	if id != LifecycleEventID("abc123", start.In(time.FixedZone("offset", 7200)), "firing") {
		t.Fatal("offset changed identity")
	}
	for _, other := range []string{
		LifecycleEventID("abc124", start, "firing"), LifecycleEventID("abc123", start, "resolved"),
		LifecycleEventID("abc123", start.Add(time.Nanosecond), "firing"),
		LifecycleEventID("abc123:firing", start, ""),
	} {
		if id == other {
			t.Fatal("distinct transition identity collided")
		}
	}
	if LifecycleEventID("a\"b", start, "c") == LifecycleEventID("a", start, "b\"c") {
		t.Fatal("ambiguous field encoding")
	}
}
