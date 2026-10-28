package consumer

import "testing"

// Inference keeps the worker count and the server-side limit in step, so extra workers can't
// sit idle behind a smaller limit, or a larger limit let more work through than the workers.
func TestInferenceConfig(t *testing.T) {
	for _, workers := range []int{1, 4, 8} {
		got := Inference(workers)
		if got.Name != "inference" || got.Workers != workers || got.MaxAckPending != workers {
			t.Errorf("Inference(%d) = %+v, want name inference, %d workers, MaxAckPending %d", workers, got, workers, workers)
		}
	}
}
