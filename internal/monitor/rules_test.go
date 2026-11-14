package monitor

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// firingAt evaluates the rules after every sample and returns, per rule and subject, the
// first time it fired.
func firingAt(cfg Config, history []Sample) map[string]time.Time {
	first := map[string]time.Time{}
	for i := range history {
		for _, f := range Evaluate(cfg, history[:i+1]) {
			key := f.Rule + "/" + f.Subject
			if _, seen := first[key]; !seen {
				first[key] = history[i].At
			}
		}
	}
	return first
}

func describe(m map[string]time.Time) string {
	var b strings.Builder
	for k, at := range m {
		fmt.Fprintf(&b, "%s at +%v; ", k, at.Sub(simStart))
	}
	return b.String()
}

func TestSteadyTrafficRaisesNoAlerts(t *testing.T) {
	s := newSim()
	s.run(70*time.Minute, 20, true)
	if got := firingAt(DefaultConfig, s.history); len(got) != 0 {
		t.Errorf("steady traffic fired: %s", describe(got))
	}
}

// The recorded M2 spike: 1,000 webhooks/s for 70 s on top of normal traffic. Lag builds and
// drains; that is the system working, so nothing may fire, at any point before, during or
// after. A watchdog that cries wolf on a normal spike gets ignored.
func TestRecordedSpikeShapeRaisesNoAlerts(t *testing.T) {
	s := newSim()
	s.run(60*time.Minute, 20, true)
	s.run(70*time.Second, 1000, true)
	s.run(20*time.Minute, 20, true)

	if got := firingAt(DefaultConfig, s.history); len(got) != 0 {
		t.Errorf("a normal spike fired: %s", describe(got))
	}
	peak := 0.0
	for _, sm := range s.history {
		c := sm.Consumers["inference"]
		peak = max(peak, float64(sm.Arrivals-c.Acked))
	}
	if peak < 40000 {
		t.Fatalf("simulated inference lag peaked at %.0f; want the M2 shape (about 45,000)", peak)
	}
}

func TestStoppedArrivalsFireWithinThirtySeconds(t *testing.T) {
	s := newSim()
	s.run(70*time.Minute, 20, true)
	down := s.at
	s.run(2*time.Minute, 0, false) // ingest down: nothing arrives, the canary included
	got := firingAt(DefaultConfig, s.history)
	at, ok := got[RuleArrivalsStopped+"/"]
	if !ok {
		t.Fatalf("arrivals stopped never fired; fired: %s", describe(got))
	}
	if d := at.Sub(down); d > 40*time.Second {
		t.Errorf("arrivals stopped fired %v after the outage, want within 30 s (+1 sample)", d)
	}
}

// With the canary turned off (CanaryMaxAge 0), its absence means nothing: arrivals stopped
// never fires, while the other rules still run.
func TestArrivalsStoppedIsOffWithoutACanary(t *testing.T) {
	cfg := DefaultConfig
	cfg.CanaryMaxAge = 0
	s := newSim()
	s.run(70*time.Minute, 20, false) // traffic, but no canary
	if got := firingAt(cfg, s.history); len(got) != 0 {
		t.Errorf("without a canary: %s", describe(got))
	}
}

func TestPartialDropFiresVolumeDrop(t *testing.T) {
	s := newSim()
	s.run(70*time.Minute, 20, true)
	s.run(10*time.Minute, 2, true) // 90% of traffic gone; the canary still arrives
	got := firingAt(DefaultConfig, s.history)
	if _, ok := got[RuleVolumeDrop+"/"]; !ok {
		t.Errorf("a 90%% drop didn't fire volume drop; fired: %s", describe(got))
	}
	if _, ok := got[RuleArrivalsStopped+"/"]; ok {
		t.Error("arrivals stopped fired although the canary kept arriving")
	}
}

// The injected failure: attribution stays up and acknowledges everything, but writes nothing.
func TestAckedButNotWrittenFiresWithinAMinute(t *testing.T) {
	s := newSim()
	s.run(70*time.Minute, 20, true)
	broke := s.at
	s.consumers["attribution"].writeFrac = 0
	s.run(3*time.Minute, 20, true)
	got := firingAt(DefaultConfig, s.history)
	at, ok := got[RuleAckedNotWritten+"/attribution"]
	if !ok {
		t.Fatalf("acked-but-not-written never fired; fired: %s", describe(got))
	}
	if d := at.Sub(broke); d > 60*time.Second {
		t.Errorf("fired %v after the failure, want within 60 s", d)
	}
	if _, ok := got[RuleAckedNotWritten+"/inference"]; ok {
		t.Error("fired for inference, which was writing normally")
	}
}

// Dead-lettered messages are acknowledged without a row, on purpose; they mustn't look like
// silent loss.
func TestDeadLettersAreNotSilentLoss(t *testing.T) {
	s := newSim()
	s.consumers["attribution"].writeFrac, s.consumers["attribution"].dlqFrac = 0.5, 0.5
	s.run(70*time.Minute, 20, true)
	if got := firingAt(DefaultConfig, s.history); len(got) != 0 {
		t.Errorf("dead letters fired: %s", describe(got))
	}
}

func TestStuckConsumerFiresStaleBacklog(t *testing.T) {
	s := newSim()
	s.run(70*time.Minute, 20, true)
	stuck := s.at
	s.consumers["inference"].capacity = 0
	s.run(8*time.Minute, 20, true)
	got := firingAt(DefaultConfig, s.history)
	at, ok := got[RuleStaleBacklog+"/inference"]
	if !ok {
		t.Fatalf("stale backlog never fired; fired: %s", describe(got))
	}
	if d := at.Sub(stuck); d < 5*time.Minute || d > 5*time.Minute+10*time.Second {
		t.Errorf("fired %v after the consumer stuck, want just after 5 minutes", d)
	}
}

// Before there is an hour of history the volume rule has no baseline and stays quiet, and a
// monitor that has only just started doesn't report a missing canary straight away.
func TestRulesWaitForEnoughHistory(t *testing.T) {
	s := newSim()
	s.run(10*time.Second, 0, false)
	if got := Evaluate(DefaultConfig, s.history); len(got) != 0 {
		t.Errorf("fired with 10 s of history: %+v", got)
	}
}

// A sustained burst (5 minutes at 1,000/s, like the step-up benchmark) lifts an hour's mean
// rate about five-fold, so normal traffic afterwards would look like an 80% drop against a
// mean baseline. The median of per-minute rates barely moves. Inference is given the
// capacity to keep up here, so only the volume rule is under test.
func TestSustainedBurstDoesNotSkewTheVolumeBaseline(t *testing.T) {
	s := newSim()
	s.consumers["inference"].capacity = 1100
	s.run(60*time.Minute, 20, true)
	s.run(5*time.Minute, 1000, true)
	s.run(20*time.Minute, 20, true)
	if got := firingAt(DefaultConfig, s.history); len(got) != 0 {
		t.Errorf("a sustained burst fired: %s", describe(got))
	}
}

// A consumer created after the monitor started (attribution started second, say) has no
// reading at the start of the window; it must not be compared against zero. Its ack floor is a
// stream position (here it starts at new messages, 50,000 in), while its row count starts at 0.
func TestConsumerAppearingMidWindowDoesNotFire(t *testing.T) {
	s := newSim()
	s.run(70*time.Minute, 20, true)
	n := len(s.history)
	for i := range s.history {
		if i < n-3 {
			delete(s.history[i].Consumers, "attribution")
			continue
		}
		c := s.history[i].Consumers["attribution"]
		c.Acked += 50000
		s.history[i].Consumers["attribution"] = c
	}
	if got := firingAt(DefaultConfig, s.history); len(got) != 0 {
		t.Errorf("a newly appeared consumer fired: %s", describe(got))
	}
}
