package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/andersh/alertmanager_event_receiver/internal/slaproxy"
)

var version = "unknown"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, output io.Writer) int {
	flags := flag.NewFlagSet("alert-sla-proxy", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("config", "alert-sla-proxy.yaml", "YAML configuration file")
	printVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(output, "unexpected positional arguments")
		return 1
	}
	if *printVersion {
		fmt.Fprintln(output, version)
		return 0
	}
	logger := slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: slog.LevelInfo}))
	file, err := os.Open(*path)
	if err != nil {
		logger.Error("cannot open configuration", "component", "config", "operation", "open", "error", err.Error())
		return 1
	}
	cfg, err := slaproxy.LoadConfig(file)
	_ = file.Close()
	if err != nil {
		logger.Error("invalid configuration", "component", "config", "operation", "validate", "error", err.Error())
		return 1
	}
	service, err := slaproxy.New(cfg, logger)
	if err != nil {
		logger.Error("cannot initialize service", "component", "sla_proxy", "operation", "initialize", "error", err.Error())
		return 1
	}
	if err := service.ListenAndServe(ctx); err != nil {
		logger.Error("SLA proxy failed", "component", "http", "operation", "serve", "error", err.Error())
		return 1
	}
	return 0
}
