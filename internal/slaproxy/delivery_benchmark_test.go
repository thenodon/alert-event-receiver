//go:build integration

package slaproxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every benchmark operation commits one firing receipt through the real logging
// sink. Setup/acceptance is untimed except in the explicitly mixed workload.
func BenchmarkDelivery(b *testing.B) {
	for _, mode := range []string{"Serial", "Parallel", "MultiInstance", "SameOccurrence", "WebhookAndDelivery", "ReconciliationUnderLoad"} {
		b.Run(mode, func(b *testing.B) {
			f := realValkey(b)
			cfg := integrationConfig(b, f)
			cfg.Durable.Scheduler.OperationTimeout = 5 * time.Second
			cfg.Durable.Scheduler.LeaseDuration = 30 * time.Second
			cfg.Durable.Scheduler.BatchSize = 10
			cfg.Delivery.Enabled = true
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
			ids := make([]string, b.N)
			for i := range ids {
				a := alertAt(b, "firing", "sla_id", "2026-01-05T08:30:00+01:00")
				a.Fingerprint = fmt.Sprintf("deliver-%d", i)
				accepted, err := services[0].durable.accept(context.Background(), a, modelsPayload(a))
				if err != nil {
					b.Fatal(err)
				}
				ids[i] = accepted.Occurrence.ID
			}
			for _, h := range hooks {
				h.reset()
			}
			var sequence atomic.Int64
			var latency atomic.Int64
			var background sync.WaitGroup
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b.ReportAllocs()
			b.ResetTimer()
			if mode == "ReconciliationUnderLoad" {
				background.Go(func() {
					var pending []string
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
							op, stop := context.WithTimeout(ctx, 5*time.Second)
							err := services[0].durable.maintenance(op, &pending)
							stop()
							if err != nil && ctx.Err() == nil {
								b.Errorf("maintenance: %v", err)
								return
							}
						}
					}
				})
			}
			run := func(i int) {
				began := time.Now()
				s := services[i%count]
				switch mode {
				case "SameOccurrence":
					var competitors sync.WaitGroup
					for range 4 {
						competitors.Go(func() {
							if err := s.dispatcher.runOne(ctx, ids[i]); err != nil {
								b.Error(err)
							}
						})
					}
					competitors.Wait()
				case "WebhookAndDelivery":
					var both sync.WaitGroup
					both.Go(func() {
						if err := s.dispatcher.runOne(ctx, ids[i]); err != nil {
							b.Error(err)
						}
					})
					both.Go(func() {
						a := alertAt(b, "firing", "sla_id", "2026-01-05T08:30:00+01:00")
						a.Fingerprint = fmt.Sprintf("ingest-%d", i)
						if w := post(s, testPayload(b, a)); w.Code != 200 {
							b.Errorf("acceptance HTTP %d: %s", w.Code, w.Body)
						}
					})
					both.Wait()
				default:
					if err := s.dispatcher.runOne(ctx, ids[i]); err != nil {
						b.Error(err)
					}
				}
				latency.Add(time.Since(began).Nanoseconds())
			}
			if mode == "Serial" || mode == "SameOccurrence" {
				for i := 0; i < b.N; i++ {
					run(i)
				}
			} else {
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						run(int(sequence.Add(1) - 1))
					}
				})
			}
			cancel()
			background.Wait()
			b.StopTimer()
			var commands, watches, retries, records int64
			for _, h := range hooks {
				commands += h.commands.Load()
				watches += h.attempts.Load()
				retries += h.retries.Load()
				records += h.records.Load()
			}
			receipts, busy, fenced, failed := float64(0), float64(0), float64(0), float64(0)
			for _, s := range services {
				families, err := s.metrics.registry.Gather()
				if err != nil {
					b.Fatal(err)
				}
				for _, family := range families {
					for _, metric := range family.Metric {
						if family.GetName() == "sla_proxy_delivery_receipts_total" {
							receipts += metric.GetCounter().GetValue()
						}
						if family.GetName() == "sla_proxy_delivery_claims_total" {
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
			n := float64(b.N)
			if receipts != n {
				b.Fatalf("committed receipts %v, want %d", receipts, b.N)
			}
			b.ReportMetric(n/b.Elapsed().Seconds(), "deliveries/s")
			b.ReportMetric(float64(latency.Load())/n, "delivery-ns/op")
			b.ReportMetric(float64(commands)/n, "commands/op")
			b.ReportMetric(float64(watches)/n, "WATCH/op")
			b.ReportMetric(float64(retries)/n, "retries/op")
			b.ReportMetric(float64(records)/n, "record-writes/op")
			b.ReportMetric(receipts/n, "receipts/op")
			b.ReportMetric(busy/n, "busy-claims/op")
			b.ReportMetric(fenced/n, "fenced-claims/op")
			b.ReportMetric(failed/n, "failed-claims/op")
		})
	}
}
