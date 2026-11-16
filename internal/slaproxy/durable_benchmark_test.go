//go:build integration

package slaproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Count actual client commands and optimistic conflicts, not timing estimates.
// Hooks are installed only in benchmarks, never in production.
type benchmarkCommands struct{ types, attempts, retries, pipelines, records, commands atomic.Int64 }

func (h *benchmarkCommands) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}
func (h *benchmarkCommands) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.commands.Add(1)
		if cmd.Name() == "type" {
			h.types.Add(1)
		}
		if cmd.Name() == "watch" {
			h.attempts.Add(1)
		}
		return next(ctx, cmd)
	}
}
func (h *benchmarkCommands) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.pipelines.Add(1)
		h.commands.Add(int64(len(cmds)))
		for _, cmd := range cmds {
			if cmd.Name() == "type" {
				h.types.Add(1)
			}
		}
		err := next(ctx, cmds)
		if errors.Is(err, redis.TxFailedErr) {
			h.retries.Add(1)
		}
		if err == nil {
			for _, cmd := range cmds {
				if cmd.Name() == "set" && len(cmd.Args()) > 1 && strings.Contains(fmt.Sprint(cmd.Args()[1]), ":occ:") {
					h.records.Add(1)
				}
			}
		}
		return err
	}
}
func (h *benchmarkCommands) reset() {
	h.types.Store(0)
	h.attempts.Store(0)
	h.retries.Store(0)
	h.pipelines.Store(0)
	h.records.Store(0)
	h.commands.Store(0)
}
func (h *benchmarkCommands) report(b *testing.B) {
	n := float64(b.N)
	b.ReportMetric(float64(h.commands.Load())/n, "commands/op")
	b.ReportMetric(float64(h.types.Load())/n, "TYPE/op")
	b.ReportMetric(float64(h.attempts.Load())/n, "WATCH/op")
	b.ReportMetric(float64(h.retries.Load())/n, "retries/op")
	b.ReportMetric(float64(h.pipelines.Load())/n, "pipelines/op")
	b.ReportMetric(float64(h.records.Load())/n, "record-writes/op")
	b.ReportMetric(n/b.Elapsed().Seconds(), "ops/s")
}
func benchmarkService(b *testing.B) (*Service, *benchmarkCommands) {
	b.Helper()
	f := realValkey(b)
	cfg := integrationConfig(b, f)
	// Use production budgets instead of the deliberately short integration leases.
	cfg.Durable.Scheduler.OperationTimeout = 5 * time.Second
	cfg.Durable.Scheduler.LeaseDuration = 30 * time.Second
	cfg.Durable.Scheduler.BatchSize = 100
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	h := &benchmarkCommands{}
	s.durable.store.client.AddHook(h)
	return s, h
}

func BenchmarkDurableWebhook(b *testing.B) {
	for _, mode := range []string{"SerialNew", "ParallelIndependent", "ParallelSameOccurrence"} {
		b.Run(mode, func(b *testing.B) {
			s, h := benchmarkService(b)
			a := alertAt(b, "firing", "sla_id", "2026-01-05T08:30:00+01:00")
			if mode == "ParallelSameOccurrence" {
				if w := post(s, testPayload(b, a)); w.Code != 200 {
					b.Fatal(w.Code, w.Body)
				}
			}
			var seq atomic.Uint64
			h.reset()
			b.ReportAllocs()
			b.ResetTimer()
			request := func() {
				alert := a
				if mode != "ParallelSameOccurrence" {
					alert.Fingerprint = fmt.Sprintf("bench-%d", seq.Add(1))
				}
				w := post(s, testPayload(b, alert))
				if w.Code != 200 {
					b.Errorf("HTTP %d: %s", w.Code, w.Body)
				}
			}
			if mode == "SerialNew" {
				for i := 0; i < b.N; i++ {
					request()
				}
			} else {
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						request()
					}
				})
			}
			b.StopTimer()
			h.report(b)
		})
	}
}

func BenchmarkDurableScheduler(b *testing.B) {
	s, h := benchmarkService(b)
	m := s.durable
	base := pendingSnapshot(b, m)
	encoded, err := json.Marshal(base)
	if err != nil {
		b.Fatal(err)
	}
	ids := make([]string, b.N)
	for i := range ids {
		var row DurableOccurrence
		if err := json.Unmarshal(encoded, &row); err != nil {
			b.Fatal(err)
		}
		row.Fingerprint = fmt.Sprintf("scheduler-%d", i)
		row.ID = tupleID(row.Fingerprint, row.StartsAt.UTC().Format(time.RFC3339Nano))
		for id, binding := range row.Bindings {
			delete(row.Bindings, id)
			binding.ID = bindingKeyID(row.ID, binding.Policy, binding.SinkID)
			row.Bindings[binding.ID] = binding
			break
		}
		ids[i] = row.ID
		if _, err := m.store.update(context.Background(), row.ID, func(*DurableOccurrence, time.Time) (*DurableOccurrence, error) { return &row, nil }); err != nil {
			b.Fatal(err)
		}
	}
	h.reset()
	b.ReportAllocs()
	b.ResetTimer()
	for _, id := range ids {
		if err := m.runOne(context.Background(), id); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	h.report(b)
}

func BenchmarkDurableReconciliation(b *testing.B) {
	s, h := benchmarkService(b)
	m := s.durable
	for i := 0; i < 100; i++ {
		a := alertAt(b, "firing", "sla_id", "2026-01-05T08:30:00+01:00")
		a.Fingerprint = fmt.Sprintf("reconcile-%d", i)
		if _, err := m.accept(context.Background(), a, modelsPayload(a)); err != nil {
			b.Fatal(err)
		}
	}
	var pending []string
	h.reset()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := m.maintenance(context.Background(), &pending); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	h.report(b)
}

// Read-only control: isolate the validation round trips from AOF fsync costs.
func BenchmarkDurableTypeValidation(b *testing.B) {
	s, _ := benchmarkService(b)
	ctx := context.Background()
	keys := []string{"occ:sample", "claim:sample", "policies", "due", "retention", "unresolved", "pending", "held", "intent-index"}
	for _, pipelined := range []bool{false, true} {
		name := "Sequential"
		if pipelined {
			name = "Pipelined"
		}
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if pipelined {
					_, err := s.durable.store.client.Pipelined(ctx, func(p redis.Pipeliner) error {
						for _, key := range keys {
							p.Type(ctx, s.durable.store.key(key))
						}
						return nil
					})
					if err != nil {
						b.Fatal(err)
					}
				} else {
					for _, key := range keys {
						if err := s.durable.store.client.Type(ctx, s.durable.store.key(key)).Err(); err != nil {
							b.Fatal(err)
						}
					}
				}
			}
		})
	}
}
