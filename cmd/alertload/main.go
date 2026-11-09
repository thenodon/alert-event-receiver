package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/andersh/alertmanager_event_receiver/internal/alertload"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := alertload.ParseConfig(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "alertload configuration:", err)
		return 2
	}
	if cfg.Remote() {
		fmt.Fprintln(stderr, "WARNING: REMOTE LOAD ENABLED. Requests modify receiver state and may create downstream events. Use a dedicated test receiver and Valkey instance.")
	}
	fmt.Fprintf(stderr, "alertload run=%s target=%s scenario=%s; duration applies to EACH phase; synthetic resolve-after=%s\n", cfg.RunID, cfg.Endpoint(), cfg.Scenario, cfg.ResolveAfter)
	report, err := alertload.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, "alertload:", err)
		return 2
	}
	if err := alertload.WriteSummary(stdout, report); err != nil {
		return 1
	}
	if cfg.Output != "" {
		file, err := os.OpenFile(cfg.Output, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			fmt.Fprintln(stderr, "cannot open JSON report output")
			return 1
		}
		writeErr := alertload.WriteJSON(file, report)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			fmt.Fprintln(stderr, "cannot write JSON report output")
			return 1
		}
	}
	if report.Interrupted {
		return 130
	}
	if report.Failed {
		return 1
	}
	return 0
}
