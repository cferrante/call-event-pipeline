//go:build integration

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The fault exists only in binaries built with the faultinject tag. The tagged build is the
// control: without it, "symbol absent" would also pass if the symbol were simply misnamed.
func TestFaultInjectionOnlyInTaggedBuilds(t *testing.T) {
	has := func(bin string) bool {
		out, err := exec.Command("go", "tool", "nm", bin).CombinedOutput()
		if err != nil {
			t.Fatalf("go tool nm %s: %v\n%s", bin, err, out)
		}
		return strings.Contains(string(out), "silentWrites")
	}
	if !has(attributionFaultBin) {
		t.Fatal("the faultinject build lacks silentWrites; the absence check below would prove nothing")
	}
	if has(attributionBin) {
		t.Error("a normal build of attribution contains the fault injection code")
	}
}

type sinkMessage struct{ Subject string }

func sinkMessages(addr string) []sinkMessage {
	resp, err := http.Get("http://" + addr + "/messages")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var msgs []sinkMessage
	_ = json.NewDecoder(resp.Body).Decode(&msgs)
	return msgs
}

// The drill: under steady load, SIGUSR1 makes attribution acknowledge without writing. The
// process stays up and its lag stays at zero, yet the monitor fires "acked but not written"
// and an alert email reaches the mail sink. A second SIGUSR1 ends the fault and the alert
// resolves. The monitor's window is shortened to 3 s so the test takes seconds; the 60-second
// target with default settings is measured on the server.
func TestInjectedSilentFailureIsDetectedAndEmailed(t *testing.T) {
	e := setup(t)
	attribution := e.startAttributionBin(t, attributionFaultBin)
	ingest := e.startIngest(t)

	const key = "SG.e2e-key"
	sinkAddr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	start(t, "mailsink", mailsinkBin, "MAILSINK_ADDR="+sinkAddr, "SENDGRID_API_KEY="+key)
	monAddr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	mon := start(t, "monitor", monitorBin,
		"DATABASE_URL="+e.dbURL, "NATS_URL="+e.natsURL, "MONITOR_ADDR="+monAddr,
		"INGEST_URL="+e.publicURL(), "INGEST_PUBLIC_URL="+e.publicURL(), "TWILIO_AUTH_TOKEN="+authToken,
		"MONITOR_INTERVAL=200ms", "CANARY_INTERVAL=300ms", "CANARY_MAX_AGE=10s", "MONITOR_WRITE_WINDOW=3s",
		"ALERT_EMAIL_TO=oncall@example.com", "ALERT_EMAIL_FROM=monitor@example.com",
		"SENDGRID_BASE_URL=http://"+sinkAddr, "SENDGRID_API_KEY="+key)

	// Steady load: about 100 calls a second until the test ends.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			e.send(t, "voice", map[string][]string{"CallSid": {fmt.Sprintf("CAfault%06d", i)},
				"From": {"+14155550100"}, "To": {"+18005550199"}})
			time.Sleep(10 * time.Millisecond)
		}
	}()
	defer func() { cancel(); wg.Wait() }()

	// A full healthy window first, so the rule has readings from before the fault.
	eventually(t, "rows flowing with a healthy monitor", func() bool {
		st, ok := e.monitorStatus(t, monAddr)
		return ok && st.Healthy && e.count(t, "SELECT count(*) FROM interactions") >= 300
	})
	time.Sleep(3 * time.Second)
	if st, _ := e.monitorStatus(t, monAddr); !st.Healthy {
		t.Fatalf("not healthy before the fault: %+v", st.Active)
	}

	if err := attribution.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	injected := time.Now()
	firing := func() bool {
		st, _ := e.monitorStatus(t, monAddr)
		return slices.ContainsFunc(st.Active, func(a struct{ Rule string }) bool { return a.Rule == "acked_not_written" })
	}
	eventually(t, "acked_not_written to fire", firing)
	t.Logf("detected %v after the fault was injected (window 3s, readings every 200ms)", time.Since(injected).Round(10*time.Millisecond))
	if attribution.exited.Load() {
		t.Fatal("attribution exited; the fault must leave it running")
	}
	if !strings.Contains(attribution.out.String(), "FAULT INJECTION") {
		t.Error("attribution didn't log that the fault is on")
	}
	eventually(t, "the alert email at the mail sink", func() bool {
		return slices.ContainsFunc(sinkMessages(sinkAddr), func(m sinkMessage) bool {
			return m.Subject == "[FIRING] acked_not_written (attribution)"
		})
	})

	if err := attribution.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	eventually(t, "acked_not_written to resolve", func() bool { return !firing() })

	cancel()
	wg.Wait()
	mon.stop(t)
	ingest.stop(t)
	attribution.stop(t)
}
