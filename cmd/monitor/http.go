package main

import (
	"embed"
	"encoding/json"
	"net/http"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/monitor"
)

// web holds the status page: HTML, script and styles as separate files, so the page runs
// under a strict Content-Security-Policy with nothing inline.
//
//go:embed web
var web embed.FS

// statusView is /status's JSON: the service's status with durations in seconds, for the page.
type statusView struct {
	At             time.Time
	Healthy        bool // no alert firing and the last reading succeeded
	ArrivalsPerSec float64
	Consumers      map[string]consumerView
	Active         []alertView
	Recent         []eventView
	SampleError    string `json:",omitempty"`
	EmailOn        bool
	Outbox         []deliveryView
}

type deliveryView struct {
	Event, Rule, Subject, State, LastError string
	Attempts                               int
	Updated                                time.Time
}

type consumerView struct {
	Lag                  uint64
	OldestPendingSeconds float64
	WrittenPerSec        float64
}

type alertView struct {
	Rule, Subject, Detail string
	Since                 time.Time
}

type eventView struct {
	State, Rule, Subject, Detail string
	At                           time.Time
	DurationSeconds              float64 `json:",omitempty"`
}

func view(st monitor.Status, outbox []monitor.Delivery, emailOn bool) statusView {
	v := statusView{At: st.At, ArrivalsPerSec: st.ArrivalsPerSec, SampleError: st.SampleError,
		Consumers: map[string]consumerView{}, Active: []alertView{}, Recent: []eventView{},
		EmailOn: emailOn, Outbox: []deliveryView{}}
	for _, d := range outbox {
		v.Outbox = append(v.Outbox, deliveryView{Event: string(d.Event.State), Rule: d.Event.Rule, Subject: d.Event.Subject,
			State: string(d.State), LastError: d.LastError, Attempts: d.Attempts, Updated: d.Updated})
	}
	for name, c := range st.Consumers {
		v.Consumers[name] = consumerView{Lag: c.Lag, OldestPendingSeconds: c.OldestPendingAge.Seconds(),
			WrittenPerSec: c.WrittenPerSec}
	}
	for _, a := range st.Active {
		v.Active = append(v.Active, alertView{a.Rule, a.Subject, a.Detail, a.Since})
	}
	for _, e := range st.Recent {
		v.Recent = append(v.Recent, eventView{string(e.State), e.Rule, e.Subject, e.Detail, e.At, e.Duration.Seconds()})
	}
	v.Healthy = len(v.Active) == 0 && v.SampleError == ""
	return v
}

// handler serves the status page and its JSON. The page polls /status every 2 seconds.
// outbox lists alert emails; nil means email alerts are off.
func handler(svc *monitor.Service, outbox func() []monitor.Delivery) http.Handler {
	mux := http.NewServeMux()
	for path, file := range map[string]string{"/{$}": "web/index.html", "/page.js": "web/page.js", "/page.css": "web/page.css"} {
		body, _ := web.ReadFile(file) // embedded at build time, so it can't be missing
		ctype := map[string]string{"/{$}": "text/html; charset=utf-8", "/page.js": "text/javascript; charset=utf-8",
			"/page.css": "text/css; charset=utf-8"}[path]
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write(body)
		})
	}
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		var deliveries []monitor.Delivery
		if outbox != nil {
			deliveries = outbox()
		}
		_ = json.NewEncoder(w).Encode(view(svc.Status(), deliveries, outbox != nil))
	})
	return mux
}
