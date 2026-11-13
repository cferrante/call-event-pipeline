package main

import (
	_ "embed"
	"encoding/json"
	"math"
	"slices"
	"time"
)

// labelled.json: synthetic SMS and email texts, each labelled with the intent a person would
// give it. Some are worded so the stub's keyword rules get them wrong, which is the gap a real
// model should close. The labels are judgement calls, made before any model was run.
//
//go:embed labelled.json
var labelledJSON []byte

type sample struct {
	Channel string `json:"channel"`
	Intent  string `json:"intent"`
	Text    string `json:"text"`
}

func loadSet() ([]sample, error) {
	var set []sample
	err := json.Unmarshal(labelledJSON, &set)
	return set, err
}

// percentile is the nearest-rank percentile p (0–100] of ds, or 0 for no samples.
func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Sorted(slices.Values(ds))
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	return s[max(rank, 1)-1]
}

// accuracy is the share of got that matches want.
func accuracy(want, got []string) float64 {
	right := 0
	for i := range want {
		if got[i] == want[i] {
			right++
		}
	}
	return float64(right) / float64(len(want))
}
