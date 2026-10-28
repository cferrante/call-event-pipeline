//go:build integration

// Package e2e runs the real ingest and attribution binaries as separate processes against a
// throwaway NATS server and database, and drives them over HTTP as Twilio would.
//
// Run with -count=1: the binaries are built with "go build", so Go's test cache doesn't know
// these tests depend on cmd/ingest and cmd/attribution and can report a stale pass.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/cferrante/decoupled-ingestion-layer/internal/dbtest"
	"github.com/cferrante/decoupled-ingestion-layer/internal/natstest"
	"github.com/cferrante/decoupled-ingestion-layer/internal/twilio"
)

const authToken = "e2e-token"

var ingestBin, attributionBin, inferenceBin string

// TestMain builds both services once for all tests.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ingestBin = filepath.Join(dir, "ingest")
	attributionBin = filepath.Join(dir, "attribution")
	inferenceBin = filepath.Join(dir, "inference")
	for bin, pkg := range map[string]string{
		ingestBin: "../cmd/ingest", attributionBin: "../cmd/attribution", inferenceBin: "../cmd/inference",
	} {
		if out, err := exec.Command("go", "build", "-o", bin, pkg).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", pkg, err, out)
			os.Exit(1)
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// syncBuffer collects a process's output; the process writes while the test may read.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type process struct {
	name   string
	cmd    *exec.Cmd
	out    *syncBuffer
	done   chan error
	exited atomic.Bool
}

// start runs bin with only the given environment (plus PATH), and stops it when the test ends.
func start(t *testing.T, name, bin string, env ...string) *process {
	t.Helper()
	return startArgs(t, name, bin, nil, env...)
}

// startArgs is start with command-line arguments.
func startArgs(t *testing.T, name, bin string, args []string, env ...string) *process {
	t.Helper()
	p := &process{name: name, cmd: exec.Command(bin, args...), out: &syncBuffer{}, done: make(chan error, 1)}
	p.cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go func() {
		err := p.cmd.Wait()
		p.exited.Store(true)
		p.done <- err
	}()
	t.Cleanup(func() {
		if p.cmd.ProcessState == nil {
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		if t.Failed() {
			t.Logf("%s output:\n%s", name, p.out.String())
		}
	})
	return p
}

// stop sends Ctrl-C and requires a clean exit within the shutdown window.
func (p *process) stop(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal %s: %v", p.name, err)
	}
	select {
	case err := <-p.done:
		if err != nil {
			t.Errorf("%s exited with %v; output:\n%s", p.name, err, p.out.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("%s did not exit after Ctrl-C", p.name)
	}
}

type env struct {
	natsURL, dbURL string
	js             jetstream.JetStream
	db             *pgxpool.Pool
	ingestPort     int
}

func setup(t *testing.T) env {
	t.Helper()
	nc := natstest.Start(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	db := dbtest.Empty(t) // the services apply migrations themselves
	return env{natsURL: nc.ConnectedUrl(), dbURL: db.Config().ConnString(), js: js, db: db, ingestPort: freePort(t)}
}

func (e env) publicURL() string { return "http://127.0.0.1:" + strconv.Itoa(e.ingestPort) }

func (e env) startIngest(t *testing.T) *process {
	t.Helper()
	p := start(t, "ingest", ingestBin,
		"INGEST_PORT="+strconv.Itoa(e.ingestPort),
		"INGEST_PUBLIC_URL="+e.publicURL(),
		"TWILIO_AUTH_TOKEN="+authToken,
		"NATS_URL="+e.natsURL,
	)
	eventually(t, "ingest to serve /healthz", func() bool {
		resp, err := http.Get(e.publicURL() + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	return p
}

func (e env) startAttribution(t *testing.T) *process {
	t.Helper()
	p := start(t, "attribution", attributionBin, "DATABASE_URL="+e.dbURL, "NATS_URL="+e.natsURL)
	eventually(t, "attribution to apply migrations", func() bool {
		return e.count(t, "SELECT count(*) FROM pg_tables WHERE tablename = 'interactions'") == 1
	})
	return p
}

// startInference runs the inference service with a fast model, so tests don't wait on it, and
// waits until its consumer exists. If the process exits instead, the test fails with its output.
func (e env) startInference(t *testing.T, args ...string) *process {
	t.Helper()
	p := startArgs(t, "inference", inferenceBin, args,
		"DATABASE_URL="+e.dbURL, "NATS_URL="+e.natsURL, "MODEL_LATENCY=10ms")
	eventually(t, "the inference consumer", func() bool {
		if p.exited.Load() {
			t.Fatalf("inference exited during startup:\n%s", p.out.String())
		}
		_, err := e.js.Consumer(context.Background(), "LEADS", "inference")
		return err == nil
	})
	return p
}

// sendCall posts a voice webhook signed as Twilio would sign it, returning the status code.
func (e env) sendCall(t *testing.T, callSid, from, to string) int {
	t.Helper()
	return e.send(t, "voice", url.Values{"CallSid": {callSid}, "From": {from}, "To": {to}})
}

// sendSMS posts a signed SMS webhook, returning the status code.
func (e env) sendSMS(t *testing.T, messageSid, from, to, body string) int {
	t.Helper()
	return e.send(t, "sms", url.Values{"MessageSid": {messageSid}, "From": {from}, "To": {to}, "Body": {body}})
}

func (e env) send(t *testing.T, channel string, form url.Values) int {
	t.Helper()
	target := e.publicURL() + "/webhooks/" + channel
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", twilio.Signature(authToken, target, form))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send %s webhook: %v", channel, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (e env) count(t *testing.T, query string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := e.db.QueryRow(ctx, query).Scan(&n); err != nil {
		return -1 // table may not exist yet
	}
	return n
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestWebhookBecomesAttributedInteraction(t *testing.T) {
	e := setup(t)
	attribution := e.startAttribution(t)
	if _, err := e.db.Exec(context.Background(),
		`INSERT INTO tracking_numbers (phone_e164, campaign_id, source) VALUES ('+18005550001', 'search-fall', 'google')`,
	); err != nil {
		t.Fatalf("seed tracking number: %v", err)
	}
	ingest := e.startIngest(t)

	if code := e.sendCall(t, "CAe2e1", "+14158675310", "+18005550001"); code != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200", code)
	}

	eventually(t, "the interaction to be attributed", func() bool {
		return e.count(t, `SELECT count(*) FROM interactions
			WHERE provider_id = 'CAe2e1' AND campaign_id = 'search-fall' AND lead_id IS NOT NULL`) == 1
	})
	if n := e.count(t, `SELECT count(*) FROM leads WHERE phone_e164 = '+14158675310' AND first_source = 'google'`); n != 1 {
		t.Errorf("leads for caller = %d, want 1", n)
	}

	ingest.stop(t)
	attribution.stop(t)
}

// The core promise: while attribution is down, webhooks are still accepted, and every one
// reaches Postgres once it's back.
func TestWebhooksAcceptedWhileAttributionIsDownAreProcessedLater(t *testing.T) {
	e := setup(t)
	ingest := e.startIngest(t)

	const n = 20
	for i := range n {
		if code := e.sendCall(t, fmt.Sprintf("CAdown%02d", i), "+14158675310", "+18005550001"); code != http.StatusOK {
			t.Fatalf("webhook %d status = %d, want 200 while attribution is down", i, code)
		}
	}

	attribution := e.startAttribution(t)
	eventually(t, "the backlog to drain into Postgres", func() bool {
		return e.count(t, "SELECT count(*) FROM interactions") == n
	})

	ingest.stop(t)
	attribution.stop(t)
}

func TestSMSWebhookProducesInteractionAndInsight(t *testing.T) {
	e := setup(t)
	attribution := e.startAttribution(t)
	inference := e.startInference(t)
	ingest := e.startIngest(t)

	if code := e.sendSMS(t, "SMe2e1", "+14158675310", "+18005550001", "What's the price for two?"); code != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200", code)
	}

	eventually(t, "the insight", func() bool {
		return e.count(t, "SELECT count(*) FROM insights WHERE provider_id = 'SMe2e1' AND intent = 'sales'") == 1
	})
	eventually(t, "the interaction", func() bool {
		return e.count(t, "SELECT count(*) FROM interactions WHERE provider_id = 'SMe2e1'") == 1
	})

	ingest.stop(t)
	inference.stop(t)
	attribution.stop(t)
}

// Bulkhead: with the inference process down, webhooks are still accepted and attributed; its
// consumer keeps its place, so inference works through the backlog once it's back.
func TestInferenceDownDoesNotAffectIngestOrAttribution(t *testing.T) {
	e := setup(t)
	attribution := e.startAttribution(t)
	e.startInference(t).stop(t) // the consumer exists; the process is down
	ingest := e.startIngest(t)

	const n = 10
	for i := range n {
		if code := e.sendCall(t, fmt.Sprintf("CAbulk%02d", i), "+14158675310", "+18005550001"); code != http.StatusOK {
			t.Fatalf("webhook %d status = %d, want 200 while inference is down", i, code)
		}
	}
	eventually(t, "attribution to record every call", func() bool {
		return e.count(t, "SELECT count(*) FROM interactions") == n
	})
	if got := e.count(t, "SELECT count(*) FROM insights"); got != 0 {
		t.Fatalf("insights = %d before inference started, want 0", got)
	}

	inference := e.startInference(t) // restarted
	eventually(t, "inference to work through its backlog", func() bool {
		return e.count(t, "SELECT count(*) FROM insights") == n
	})

	ingest.stop(t)
	inference.stop(t)
	attribution.stop(t)
}

// Inference can run without attribution: it applies the migrations it needs itself.
func TestInferenceStartsAloneOnAFreshDatabase(t *testing.T) {
	e := setup(t)
	inference := e.startInference(t)

	if n := e.count(t, "SELECT count(*) FROM pg_tables WHERE tablename = 'insights'"); n != 1 {
		t.Errorf("insights table missing: inference didn't apply migrations")
	}
	inference.stop(t)
}

// An inference consumer created before it started at new messages reads all history.
// Starting must fail with instructions; -reset-consumer then recreates it at new messages.
func TestInferenceResetConsumer(t *testing.T) {
	e := setup(t)
	e.startAttribution(t)
	eventually(t, "the LEADS stream", func() bool { // attribution creates it after migrating
		_, err := e.js.Stream(context.Background(), "LEADS")
		return err == nil
	})
	if _, err := e.js.CreateOrUpdateConsumer(context.Background(), "LEADS", jetstream.ConsumerConfig{
		Durable: "inference", AckPolicy: jetstream.AckExplicitPolicy, DeliverPolicy: jetstream.DeliverAllPolicy,
	}); err != nil {
		t.Fatalf("create old consumer: %v", err)
	}

	old := startArgs(t, "inference", inferenceBin, nil, "DATABASE_URL="+e.dbURL, "NATS_URL="+e.natsURL)
	select {
	case err := <-old.done:
		if err == nil || !strings.Contains(old.out.String(), "-reset-consumer") {
			t.Errorf("exit = %v, output:\n%s\nwant a failure explaining -reset-consumer", err, old.out.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("inference kept running with an old consumer")
	}

	// startInference returns once *a* consumer exists, which may still be the old one.
	inference := e.startInference(t, "-reset-consumer")
	eventually(t, "the consumer to be recreated at new messages", func() bool {
		c, err := e.js.Consumer(context.Background(), "LEADS", "inference")
		return err == nil && c.CachedInfo().Config.DeliverPolicy == jetstream.DeliverNewPolicy
	})
	inference.stop(t)
}
