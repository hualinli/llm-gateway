package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"llm-gateway/internal/metering"
)

func TestGatewayShutdownFlushesAfterCancelingLongStream(t *testing.T) {
	dir := t.TempDir()
	producer := metering.NewProducerWithOptions(nil, 4, 10, time.Hour, metering.ProducerOptions{SpoolDir: dir})
	producer.Enqueue(metering.Event{RequestID: "already-completed"})
	requests, cancelRequests := context.WithCancel(context.Background())
	drain := newRequestDrain()
	started := make(chan struct{})
	server := httptest.NewUnstartedServer(drain.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		producer.Enqueue(metering.Event{RequestID: "handler-finished-on-cancel"})
	})))
	server.Config.BaseContext = func(net.Listener) context.Context { return requests }
	server.Start()
	t.Cleanup(func() { cancelRequests(); server.Close(); _ = producer.Close(context.Background()) })
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		resp, err := server.Client().Get(server.URL)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	checked := &checkFlushContext{t: t, inner: producer}
	err := shutdownGateway(server.Config, drain, cancelRequests, checked, shutdownOptions{
		GracePeriod: 20 * time.Millisecond, CancelPeriod: time.Second, FlushPeriod: time.Second,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected drain timeout to be reported, got %v", err)
	}
	if !checked.called {
		t.Fatal("HTTP drain timeout skipped metering close")
	}
	file, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	ids := map[string]bool{}
	for {
		var event metering.Event
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		ids[event.RequestID] = true
	}
	if !ids["already-completed"] || !ids["handler-finished-on-cancel"] || len(ids) != 2 {
		t.Fatalf("metering closed before request cleanup completed: %v", ids)
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("client connection did not close")
	}
}

func TestGatewayShutdownAllowsStreamToFinishDuringGrace(t *testing.T) {
	requests, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	drain := newRequestDrain()
	started := make(chan struct{})
	server := httptest.NewUnstartedServer(drain.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-time.After(20 * time.Millisecond):
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	})))
	server.Config.BaseContext = func(net.Listener) context.Context { return requests }
	server.Start()
	defer server.Close()
	response := make(chan string, 1)
	go func() {
		resp, err := server.Client().Get(server.URL)
		if err != nil {
			response <- err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		response <- string(body)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	closer := &checkFlushContext{t: t, inner: noopCloser{}}
	if err := shutdownGateway(server.Config, drain, cancelRequests, closer, shutdownOptions{GracePeriod: time.Second, CancelPeriod: time.Second, FlushPeriod: time.Second}); err != nil {
		t.Fatal(err)
	}
	if !closer.called {
		t.Fatal("metering not closed")
	}
	select {
	case body := <-response:
		if body != "data: [DONE]\n\n" {
			t.Fatalf("graceful shutdown interrupted stream: %q", body)
		}
	case <-time.After(time.Second):
		t.Fatal("client did not complete")
	}
	late := httptest.NewRecorder()
	drain.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("new request admitted while draining") })).ServeHTTP(late, httptest.NewRequest("GET", "/", nil))
	if late.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining status=%d", late.Code)
	}
}

type noopCloser struct{}

func (noopCloser) Close(context.Context) error { return nil }

type checkFlushContext struct {
	t      *testing.T
	inner  meteringCloser
	called bool
}

func (c *checkFlushContext) Close(ctx context.Context) error {
	c.called = true
	if ctx.Err() != nil {
		c.t.Errorf("metering inherited expired HTTP drain context: %v", ctx.Err())
	}
	return c.inner.Close(ctx)
}
