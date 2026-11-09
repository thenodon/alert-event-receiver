package alertload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

type task struct {
	index         int
	due, enqueued time.Time
}
type phaseState struct {
	sync.Mutex
	report                                  PhaseReport
	inflight                                int
	latency, queue, construction, lag, size recorder
}

// Run validates configuration before any traffic. It uses only bounded in-memory
// queues, bitsets and recorders, and never retries a webhook or follows redirects.
func Run(ctx context.Context, c Config) (Report, error) {
	if err := c.Validate(); err != nil {
		return Report{}, err
	}
	start := time.Now()
	report := newReport(c)
	transport := &http.Transport{MaxIdleConns: c.Workers, MaxIdleConnsPerHost: c.Workers, MaxConnsPerHost: c.Workers, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: c.Timeout, ResponseHeaderTimeout: c.Timeout, MaxResponseHeaderBytes: 64 << 10, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: c.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	opened, closed := newBitset(c.RequestsPerPhase()), newBitset(c.RequestsPerPhase())
	specs := plan(c, &opened, &closed)
	report.PlannedMaxRequests = uint64(len(specs) * c.RequestsPerPhase())
	report.PlannedMaxAlerts = report.PlannedMaxRequests * uint64(c.BatchSize)
	for _, spec := range specs {
		if ctx.Err() != nil {
			report.Interrupted = true
			break
		}
		p := runPhase(ctx, c, client, spec)
		report.Phases = append(report.Phases, p)
		if ctx.Err() != nil {
			report.Interrupted = true
			break
		}
		if p.DrainTimedOut {
			break
		}
	}
	report.finish(c, start)
	return report, nil
}

func runPhase(ctx context.Context, c Config, client *http.Client, spec phaseSpec) PhaseReport {
	start := time.Now()
	n := c.RequestsPerPhase()
	stats := &phaseState{report: PhaseReport{Name: spec.name, Transition: spec.status, ExpectedOutcome: spec.outcome, PlannedRequests: uint64(n), ResponseClasses: make(map[string]uint64)}}
	for i := 0; i < n; i++ {
		if spec.eligible == nil || (*spec.eligible).has(i) {
			stats.report.EligibleRequests++
		}
	}
	if stats.report.EligibleRequests == 0 {
		stats.report.WallSeconds = time.Since(start).Seconds()
		return stats.report
	}
	jobs := make(chan task, c.MaxPending)
	// Cancellation stops scheduling first. In-flight work gets a bounded grace
	// period, independent of the already-cancelled caller context.
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	var workers sync.WaitGroup
	for range c.Workers {
		workers.Go(func() {
			for job := range jobs {
				send(workerCtx, c, client, spec, job, stats)
			}
		})
	}
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	schedulingStart := time.Now()
	ordinal := 0
	for i := 0; i < n; i++ {
		if spec.eligible != nil && !(*spec.eligible).has(i) {
			continue
		}
		due := schedulingStart.Add(time.Duration(float64(ordinal) / c.Rate * float64(time.Second)))
		ordinal++
		if !waitUntil(ctx, timer, due) {
			break
		}
		stats.Lock()
		stats.report.OfferedRequests++
		if time.Since(schedulingStart) >= c.Duration {
			stats.report.MissedDeadline++
			stats.Unlock()
			continue
		}
		job := task{index: i, due: due, enqueued: time.Now()}
		select {
		case jobs <- job:
			stats.report.ScheduledRequests++
		default:
			stats.report.QueueFull++
		}
		stats.Unlock()
	}
	// Preserve the configured observation window even if few eligible requests
	// remain after failures in an earlier phase.
	_ = waitUntil(ctx, timer, schedulingStart.Add(c.Duration))
	scheduleEnd := time.Now()
	stats.Lock()
	stats.report.SchedulingSeconds = scheduleEnd.Sub(schedulingStart).Seconds()
	stats.report.NotOffered = stats.report.EligibleRequests - stats.report.OfferedRequests
	stats.Unlock()
	close(jobs)
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	drain := time.NewTimer(c.DrainTimeout)
	select {
	case <-done:
	case <-drain.C:
		stats.Lock()
		stats.report.DrainTimedOut = true
		stats.Unlock()
		cancelWorkers()
		<-done
	}
	drain.Stop()
	stats.Lock()
	defer stats.Unlock()
	stats.report.DrainSeconds = time.Since(scheduleEnd).Seconds()
	stats.report.WallSeconds = time.Since(start).Seconds()
	stats.report.HTTPLatency = stats.latency.summary(float64(time.Second))
	stats.report.QueueWait = stats.queue.summary(float64(time.Second))
	stats.report.Construction = stats.construction.summary(float64(time.Second))
	stats.report.SchedulingLag = stats.lag.summary(float64(time.Second))
	stats.report.PayloadBytes = stats.size.summary(1)
	p := stats.report
	p.Rates = rates(p.OfferedRequests, p.SentRequests, p.CompletedRequests, p.SuccessfulRequests, p.SentAlerts, p.AcceptedAlerts, c.BatchSize, p.SchedulingSeconds, p.WallSeconds)
	return p
}
func waitUntil(ctx context.Context, timer *time.Timer, due time.Time) bool {
	if ctx.Err() != nil {
		return false
	}
	delay := time.Until(due)
	if delay <= 0 {
		return true
	}
	timer.Reset(delay)
	select {
	case <-timer.C:
		return ctx.Err() == nil
	case <-ctx.Done():
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		return false
	}
}

func send(ctx context.Context, c Config, client *http.Client, spec phaseSpec, job task, s *phaseState) {
	if ctx.Err() != nil {
		s.Lock()
		s.report.Abandoned++
		s.Unlock()
		return
	}
	constructionStart := time.Now()
	data, err := json.Marshal(PayloadFor(c, job.index, spec.status))
	if err != nil {
		s.Lock()
		s.report.Abandoned++
		s.report.ConstructionErrors++
		s.Unlock()
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(data))
	if err != nil {
		s.Lock()
		s.report.Abandoned++
		s.report.ConstructionErrors++
		s.Unlock()
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "alertload/1")
	constructionDuration := time.Since(constructionStart)
	s.Lock()
	s.queue.add(float64(constructionStart.Sub(job.enqueued)))
	s.lag.add(float64(job.enqueued.Sub(job.due)))
	s.construction.add(float64(constructionDuration))
	s.size.add(float64(len(data)))
	if ctx.Err() != nil {
		s.report.Abandoned++
		s.Unlock()
		return
	}
	s.report.SentRequests++
	s.report.SentAlerts += uint64(c.BatchSize)
	s.inflight++
	s.report.PeakInflight = max(s.report.PeakInflight, s.inflight)
	s.Unlock()
	began := time.Now()
	response, err := client.Do(request)
	if response != nil {
		// Read only a bounded prefix for connection reuse, never retaining or logging
		// backend error bodies that can echo credentials or arbitrary payloads.
		if err == nil {
			_, err = io.Copy(io.Discard, io.LimitReader(response.Body, c.MaxResponseBytes+1))
		}
		_ = response.Body.Close()
	}
	elapsed := time.Since(began)
	s.Lock()
	defer s.Unlock()
	s.inflight--
	s.latency.add(float64(elapsed))
	if response != nil {
		s.report.CompletedRequests++
		class := response.StatusCode / 100
		s.report.ResponseClasses[string(rune('0'+class))+"xx"]++
	}
	accepted := err == nil && response != nil && response.StatusCode >= 200 && response.StatusCode < 300
	if accepted {
		s.report.SuccessfulRequests++
		s.report.AcceptedAlerts += uint64(c.BatchSize)
		if spec.accepted != nil {
			(*spec.accepted).set(job.index)
		}
		return
	}
	s.report.FailedRequests++
	if err != nil {
		var netErr net.Error
		switch {
		case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
			s.report.Timeouts++
		case errors.Is(err, context.Canceled):
			s.report.CanceledRequests++
		default:
			s.report.NetworkErrors++
		}
	}
	if err == nil && response != nil && response.StatusCode >= 400 && response.StatusCode < 500 {
		s.report.RejectedAlerts += uint64(c.BatchSize)
	} else {
		s.report.UnknownAlerts += uint64(c.BatchSize)
	}
}
