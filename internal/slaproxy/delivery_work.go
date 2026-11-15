package slaproxy

import (
	"encoding/hex"
	"strings"
)

func deliveryWorkID(occurrenceID, bindingID string) string { return occurrenceID + ":" + bindingID }
func parseDeliveryWork(value string) (string, string, bool) {
	occurrence, binding, found := strings.Cut(value, ":")
	if !found || len(occurrence) != 64 || len(binding) != 64 {
		return "", "", false
	}
	if _, err := hex.DecodeString(occurrence); err != nil {
		return "", "", false
	}
	if _, err := hex.DecodeString(binding); err != nil {
		return "", "", false
	}
	return occurrence, binding, true
}
