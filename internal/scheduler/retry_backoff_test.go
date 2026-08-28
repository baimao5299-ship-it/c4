package scheduler

import (
	"testing"
	"time"
)

func TestRetry429DeadlineUsesProviderAndExponentialFallback(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	for i, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second} {
		got := retry429Deadline(now, uint32(i+1), nil)
		if got == nil || !got.Equal(now.Add(want)) {
			t.Fatalf("streak %d: got %v want %v", i+1, got, now.Add(want))
		}
	}

	provider := now.Add(90 * time.Second)
	got := retry429Deadline(now, 1, &provider)
	if got == nil || !got.Equal(provider) {
		t.Fatalf("provider deadline: got %v want %v", got, provider)
	}

	tooFar := now.Add(retry429Max + time.Minute)
	got = retry429Deadline(now, 1, &tooFar)
	if got == nil || !got.Equal(now.Add(retry429Max)) {
		t.Fatalf("provider cap: got %v", got)
	}
}
