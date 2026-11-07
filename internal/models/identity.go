package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// LifecycleEventID identifies one transition of an occurrence. JSON array
// encoding separates the fields unambiguously; UTC normalizes timezone offsets.
// endsAt and notification delivery time deliberately do not participate.
func LifecycleEventID(fingerprint string, startsAt time.Time, transition string) string {
	encoded, _ := json.Marshal([3]string{fingerprint, startsAt.UTC().Format(time.RFC3339Nano), transition})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
