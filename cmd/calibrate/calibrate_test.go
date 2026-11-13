package main

import (
	"regexp"
	"testing"
	"time"

	"github.com/cferrante/call-event-pipeline/internal/inference"
)

func TestPercentileIsNearestRank(t *testing.T) {
	var ds []time.Duration
	for i := 100; i >= 1; i-- { // unsorted on purpose
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	for _, tt := range []struct {
		p    float64
		want time.Duration
	}{{50, 50 * time.Millisecond}, {95, 95 * time.Millisecond}, {99, 99 * time.Millisecond}, {100, 100 * time.Millisecond}} {
		if got := percentile(ds, tt.p); got != tt.want {
			t.Errorf("p%v = %s, want %s", tt.p, got, tt.want)
		}
	}
	// With 10 samples p95 is rank 9.5, which nearest-rank rounds up to the 10th.
	var ten []time.Duration
	for i := 1; i <= 10; i++ {
		ten = append(ten, time.Duration(i)*time.Millisecond)
	}
	if got := percentile(ten, 95); got != 10*time.Millisecond {
		t.Errorf("10 samples: p95 = %s, want 10ms", got)
	}
	if got := percentile([]time.Duration{7 * time.Millisecond}, 99); got != 7*time.Millisecond {
		t.Errorf("one sample: p99 = %s", got)
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("no samples: p50 = %s, want 0", got)
	}
}

func TestAccuracy(t *testing.T) {
	got := accuracy([]string{"sales", "support", "unknown", "sales"}, []string{"sales", "sales", "unknown", "support"})
	if got != 0.5 {
		t.Fatalf("accuracy = %v, want 0.5", got)
	}
}

// The set is balanced, uses only the three intents, and every phone number and address in it
// is removed before a model sees it.
func TestLabelledSet(t *testing.T) {
	set, err := loadSet()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, s := range set {
		counts[s.Intent]++
		if s.Channel != "sms" && s.Channel != "email" {
			t.Errorf("channel %q in %q", s.Channel, s.Text)
		}
	}
	if len(counts) != 3 || counts["sales"] != counts["support"] || counts["support"] != counts["unknown"] {
		t.Fatalf("intents = %v, want sales, support and unknown in equal numbers", counts)
	}
	pii := regexp.MustCompile(`555-01\d\d|@example\.com`)
	found := 0
	for _, s := range set {
		if pii.MatchString(s.Text) {
			found++
		}
		if red := inference.Redact(s.Text); pii.MatchString(red) {
			t.Errorf("personal data survives redaction: %q", red)
		}
	}
	if found == 0 {
		t.Fatal("the set has no phone numbers or addresses, so redaction isn't exercised")
	}
}
