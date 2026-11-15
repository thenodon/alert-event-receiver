//go:build integration

package slaproxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These benchmarks include real sink I/O, lease validation and receipt commits.
// One Go operation is an occurrence; custom metrics are normalized per receipt.
func BenchmarkMultiSinkDelivery(b *testing.B) {
	for _, mode := range []string{"SerialHTTP", "TwoHTTP", "HTTPAndLog", "ConcurrentHTTP", "MultiInstance", "SameOccurrence", "Retry"} {
		b.Run(mode, func(b *testing.B) {
			f := realValkey(b)
			var attemptMu sync.Mutex
			attempts := make(map[string]int)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if mode == "Retry" {
					key := r.Header.Get("Idempotency-Key")
					attemptMu.Lock()
					attempts[key]++
					n := attempts[key]
					attemptMu.Unlock()
					if n == 1 {
						w.WriteHeader(503)
						return
					}
				}
				w.WriteHeader(202)
			})
			server := httptest.NewServer(handler)
			defer server.Close()
			other := httptest.NewServer(handler)
			defer other.Close()
			cfg := integrationConfig(b, f)
			cfg.Durable.Scheduler.OperationTimeout = 5 * time.Second
			cfg.Durable.Scheduler.LeaseDuration = 30 * time.Second
			cfg.Delivery.Enabled = true
			cfg.Delivery.Dispatcher.PollInterval = 10 * time.Millisecond
			cfg.Delivery.Dispatcher.RetryInitial = 10 * time.Millisecond
			cfg.Delivery.Dispatcher.RetryMax = 10 * time.Millisecond
			cfg.Delivery.Sinks = []SinkConfig{{ID: "http-one", Type: "http", HTTP: &HTTPSinkConfig{URL: server.URL, Timeout: time.Second}}}
			if mode == "TwoHTTP" {
				cfg.Delivery.Sinks = append(cfg.Delivery.Sinks, SinkConfig{ID: "http-two", Type: "http", HTTP: &HTTPSinkConfig{URL: other.URL, Timeout: time.Second}})
			}
			if mode == "HTTPAndLog" {
				cfg.Delivery.Sinks = append(cfg.Delivery.Sinks, SinkConfig{ID: "audit", Type: "log"})
			}
			count := 1
			if mode == "MultiInstance" {
				count = 4
			}
			services := make([]*Service, count)
			hooks := make([]*benchmarkCommands, count)
			for i := range services {
				s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { _ = s.Close() })
				services[i] = s
				hooks[i] = &benchmarkCommands{}
				s.durable.store.client.AddHook(hooks[i])
			}
			work := make([][]string, b.N)
			for i := range work {
				a := alertAt(b, "firing", "sla_id", "2026-01-05T08:30:00.123456789+01:00")
				a.Fingerprint = fmt.Sprintf("multi-%d", i)
				accepted, err := services[0].durable.accept(context.Background(), a, modelsPayload(a))
				if err != nil {
					b.Fatal(err)
				}
				for _, sink := range cfg.Delivery.Sinks {
					binding := sinkBinding(b, accepted.Occurrence, sink.ID)
					work[i] = append(work[i], deliveryWorkID(accepted.Occurrence.ID, binding.ID))
				}
			}
			for _, h := range hooks {
				h.reset()
			}
			var latency atomic.Int64
			run := func(i int) {
				s := services[i%count]
				began := time.Now()
				deliver := func(id string) {
					if err := s.dispatcher.runOne(context.Background(), id); err != nil {
						b.Error(err)
					}
				}
				for _, id := range work[i] {
					if mode == "SameOccurrence" {
						var competing sync.WaitGroup
						for range 4 {
							competing.Go(func() { deliver(id) })
						}
						competing.Wait()
					} else {
						deliver(id)
					}
					if mode == "Retry" {
						occ, bid, _ := parseDeliveryWork(id)
						row, err := s.durable.store.load(context.Background(), occ)
						if err != nil {
							b.Error(err)
							return
						}
						delay := time.Until(row.Bindings[bid].Intents["firing"].NextAttemptAt)
						if delay > 0 {
							time.Sleep(delay + time.Millisecond)
						}
						deliver(id)
					}
				}
				latency.Add(time.Since(began).Nanoseconds())
			}
			b.ReportAllocs()
			b.ResetTimer()
			if mode == "SerialHTTP" || mode == "SameOccurrence" {
				for i := range work {
					run(i)
				}
			} else {
				jobs := make(chan int)
				var workers sync.WaitGroup
				for range 4 {
					workers.Go(func() {
						for i := range jobs {
							run(i)
						}
					})
				}
				for i := range work {
					jobs <- i
				}
				close(jobs)
				workers.Wait()
			}
			b.StopTimer()
			receipts, busy, fenced, failed := float64(0), float64(0), float64(0), float64(0)
			for _, s := range services {
				families, err := s.metrics.registry.Gather()
				if err != nil {
					b.Fatal(err)
				}
				for _, family := range families {
					for _, metric := range family.Metric {
						switch family.GetName() {
						case "sla_proxy_delivery_receipts_total":
							receipts += metric.GetCounter().GetValue()
						case "sla_proxy_delivery_claims_total":
							for _, label := range metric.Label {
								if label.GetName() == "result" {
									switch label.GetValue() {
									case "busy":
										busy += metric.GetCounter().GetValue()
									case "fenced":
										fenced += metric.GetCounter().GetValue()
									case "error":
										failed += metric.GetCounter().GetValue()
									}
								}
							}
						}
					}
				}
			}
			want := float64(b.N * len(cfg.Delivery.Sinks))
			if receipts != want {
				b.Fatalf("receipts %v != %v", receipts, want)
			}
			var commands, watches, retries, records int64
			for _, h := range hooks {
				commands += h.commands.Load()
				watches += h.attempts.Load()
				retries += h.retries.Load()
				records += h.records.Load()
			}
			b.ReportMetric(receipts/b.Elapsed().Seconds(), "deliveries/s")
			b.ReportMetric(float64(latency.Load())/want, "delivery-ns/op")
			b.ReportMetric(float64(commands)/want, "commands/delivery")
			b.ReportMetric(float64(watches)/want, "WATCH/delivery")
			b.ReportMetric(float64(retries)/want, "WATCH-retries/delivery")
			b.ReportMetric(float64(records)/want, "record-writes/delivery")
			b.ReportMetric(receipts, "receipts")
			b.ReportMetric(busy/want, "busy-claims/delivery")
			b.ReportMetric(fenced/want, "fenced-claims/delivery")
			b.ReportMetric(failed/want, "failed-claims/delivery")
		})
	}
}
