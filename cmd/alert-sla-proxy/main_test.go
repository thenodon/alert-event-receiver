package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configFile(t *testing.T, address string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := fmt.Sprintf(`listen_address: %q
sla_definitions:
  - sla_label: sla_id
    slas:
      always:
        timezone: UTC
        schedule:
          monday: ["00:00-24:00"]
`, address)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommandFlagsAndConfigurationErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
		text string
	}{
		{[]string{"-version"}, 0, version},
		{[]string{"-help"}, 0, "YAML configuration"},
		{[]string{"-unknown"}, 1, "flag provided but not defined"},
		{[]string{"positional"}, 1, "unexpected positional arguments"},
		{[]string{"-config", filepath.Join(t.TempDir(), "missing.yaml")}, 1, "cannot open configuration"},
		{[]string{"-config", configFile(t, "invalid")}, 1, "invalid configuration"},
	} {
		var output bytes.Buffer
		if got := run(context.Background(), tc.args, &output); got != tc.want || !strings.Contains(output.String(), tc.text) {
			t.Fatalf("args=%v code=%d output=%s", tc.args, got, output.String())
		}
	}
}

func TestCommandListenerFailureReturnsNonzero(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var output bytes.Buffer
	if code := run(context.Background(), []string{"-config", configFile(t, l.Addr().String())}, &output); code != 1 || !strings.Contains(output.String(), `"operation":"serve"`) {
		t.Fatalf("code=%d output=%s", code, output.String())
	}
}

func TestCommandCancellationShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	if code := run(ctx, []string{"-config", configFile(t, "127.0.0.1:0")}, &output); code != 0 || !strings.Contains(output.String(), "HTTP shutdown completed") {
		t.Fatalf("code=%d output=%s", code, output.String())
	}
}
