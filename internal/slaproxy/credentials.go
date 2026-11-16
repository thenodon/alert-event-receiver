package slaproxy

import (
	"errors"
	"os"
	"strings"
)

// References are deliberately whole-value and single-pass. Other YAML fields
// are not expanded, and values obtained from the environment stay literal.
func credentialReference(value string) (name string, reference bool, err error) {
	if !strings.Contains(value, "${") {
		return "", false, nil
	}
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		name = value[2 : len(value)-1]
		if labelName.MatchString(name) {
			return name, true, nil
		}
	}
	return "", false, errors.New("basic_auth credentials must be literals or exact ${NAME} references")
}
func resolveCredential(value string) (string, error) {
	name, reference, err := credentialReference(value)
	if err != nil {
		return "", err
	}
	if !reference {
		return value, nil
	}
	resolved, exists := os.LookupEnv(name)
	if !exists || resolved == "" {
		return "", errors.New("credential environment variable is missing or empty")
	}
	return resolved, nil
}
func credentialIdentity(value string) string {
	_, reference, _ := credentialReference(value)
	if reference || value == "" {
		return value
	}
	return "<literal>"
}
