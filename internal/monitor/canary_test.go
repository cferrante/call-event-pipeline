package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cferrante/call-event-pipeline/internal/twilio"
)

// The canary is an ordinary signed voice webhook, so it proves the whole path from the edge's
// signature check to the database, and comes from "canary" so it never creates a lead.
func TestCanaryIsASignedVoiceWebhook(t *testing.T) {
	var (
		mu    sync.Mutex
		seen  []string
		valid bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		valid = r.URL.Path == "/webhooks/voice" &&
			twilio.Valid("tok", "https://hooks.example.com/webhooks/voice", r.PostForm, r.Header.Get("X-Twilio-Signature")) &&
			r.PostForm.Get("From") == CanaryFrom && r.PostForm.Get("To") == CanaryTo
		seen = append(seen, r.PostForm.Get("CallSid"))
	}))
	defer srv.Close()

	c := Canary{Target: srv.URL, PublicURL: "https://hooks.example.com", AuthToken: "tok", Client: srv.Client()}
	for range 2 {
		if err := c.Send(context.Background()); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !valid {
		t.Error("canary webhook isn't a valid signed voice webhook from the canary number")
	}
	if len(seen) != 2 || seen[0] == seen[1] || !strings.HasPrefix(seen[0], "CAcanary") {
		t.Errorf("CallSids = %v, want two distinct CAcanary… IDs (each is a new interaction)", seen)
	}
}

func TestCanaryReportsAFailedWebhook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := Canary{Target: srv.URL, PublicURL: "https://hooks.example.com", AuthToken: "tok", Client: srv.Client()}
	if err := c.Send(context.Background()); err == nil {
		t.Error("a 503 was reported as success")
	}
}
