package slaproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func listenTest(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func awaitStopped(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("server failed to stop within bound")
		return nil
	}
}

func TestShutdownDrainsInflightObservation(t *testing.T) {
	s := newTestService(t)
	l := listenTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	original := s.handler
	s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		original.ServeHTTP(w, r)
		if r.URL.Path == "/webhook" {
			close(entered)
			<-release
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	clientDone := make(chan error, 1)
	body := testPayload(t, testAlert("firing", map[string]string{"sla_id": "business"}))
	client := &http.Client{Timeout: 2 * time.Second}
	go func() {
		resp, err := client.Post("http://"+l.Addr().String()+"/webhook", "application/json", strings.NewReader(string(body)))
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = errors.New("in-flight observation failed")
			}
		}
		clientDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("request did not enter handler")
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for s.ready.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ready := s.ready.Load()
	// The response is prepared but its handler is still running. Shutdown
	// must wait for completion rather than closing this active connection.
	select {
	case err := <-done:
		close(release)
		t.Fatalf("shutdown did not drain: %v", err)
	default:
	}
	close(release)
	if ready {
		t.Error("readiness did not stop during shutdown")
	}
	if err := awaitStopped(t, clientDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitStopped(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownTimeoutClosesActiveConnection(t *testing.T) {
	s := newTestService(t)
	s.cfg.ShutdownTimeout = 20 * time.Millisecond
	l := listenTest(t)
	entered, finished := make(chan struct{}), make(chan struct{})
	s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(finished)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	clientDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: time.Second}
		resp, err := client.Get("http://" + l.Addr().String() + "/healthz")
		if resp != nil {
			resp.Body.Close()
		}
		clientDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not enter handler")
	}
	cancel()
	if err := awaitStopped(t, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected shutdown deadline, got %v", err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("active connection was not closed")
	}
	_ = awaitStopped(t, clientDone)
}

func TestUnexpectedListenerFailure(t *testing.T) {
	s := newTestService(t)
	l := listenTest(t)
	l.Close()
	if err := s.Serve(context.Background(), l); err == nil {
		t.Fatal("listener failure reported as graceful shutdown")
	}
	if s.ready.Load() {
		t.Fatal("ready after failure")
	}
}

func TestPortBindingFailure(t *testing.T) {
	l := listenTest(t)
	s := newTestService(t)
	s.cfg.ListenAddress = l.Addr().String()
	if err := s.ListenAndServe(context.Background()); err == nil {
		t.Fatal("binding failure hidden")
	}
}
