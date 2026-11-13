package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/monitor"
)

type staticSource struct{ s monitor.Sample }

func (f staticSource) Sample(_ context.Context, now time.Time) (monitor.Sample, error) {
	s := f.s
	s.At = now
	return s, nil
}

func TestStatusEndpoint(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	svc := monitor.NewService(staticSource{monitor.Sample{Arrivals: 50, CanarySeen: t0,
		Consumers: map[string]monitor.ConsumerSample{"attribution": {Acked: 40, Written: 40, OldestPending: t0}}}},
		monitor.DefaultConfig, func(monitor.Event) {})
	svc.Tick(context.Background(), t0.Add(3*time.Second))

	rec := httptest.NewRecorder()
	handler(svc, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var got statusView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	a := got.Consumers["attribution"]
	if a.Lag != 10 || a.OldestPendingSeconds != 3 || got.Healthy != true {
		t.Errorf("status = %+v", got)
	}

	rec = httptest.NewRecorder()
	handler(svc, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>Pipeline monitor</title>") {
		t.Errorf("page: status %d", rec.Code)
	}
	// Script and styles are separate same-origin files, so nothing inline may run.
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") ||
		strings.Contains(csp, "unsafe-inline") {
		t.Errorf("page CSP = %q, want default-src 'self' and nothing inline", csp)
	}
	for _, asset := range []string{"/page.js", "/page.css"} {
		rec = httptest.NewRecorder()
		handler(svc, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, asset, nil))
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("%s: status %d, %d bytes", asset, rec.Code, rec.Body.Len())
		}
	}
}

// The status page's Outbox shows each alert email and how its delivery went.
func TestStatusShowsOutbox(t *testing.T) {
	svc := monitor.NewService(staticSource{}, monitor.DefaultConfig, func(monitor.Event) {})
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	outbox := func() []monitor.Delivery {
		return []monitor.Delivery{{Event: monitor.Event{State: monitor.Firing, Rule: monitor.RuleArrivalsStopped, At: at},
			State: monitor.DeliveryRetrying, Attempts: 2, LastError: "mail send: HTTP 429", Updated: at}}
	}
	rec := httptest.NewRecorder()
	handler(svc, outbox).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var got statusView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Outbox) != 1 || got.Outbox[0].State != "retrying" || got.Outbox[0].Attempts != 2 ||
		got.Outbox[0].Rule != monitor.RuleArrivalsStopped || got.Outbox[0].Event != "firing" ||
		got.Outbox[0].LastError != "mail send: HTTP 429" || !got.EmailOn {
		t.Errorf("outbox %+v, email on %v", got.Outbox, got.EmailOn)
	}

	rec = httptest.NewRecorder()
	handler(svc, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	got = statusView{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.EmailOn || got.Outbox == nil {
		t.Errorf("without email: on %v, outbox %v; want off and an empty list", got.EmailOn, got.Outbox)
	}
}
