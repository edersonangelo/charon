package delivery

import (
	"testing"
	"time"
)

func TestBackoffGrowsAndStaysWithinTheCap(t *testing.T) {
	t.Parallel()

	base := time.Second
	limit := time.Minute

	var previous time.Duration
	for attempt := 1; attempt <= 12; attempt++ {
		got := backoff(attempt, base, limit)

		if got <= 0 {
			t.Fatalf("backoff(%d) = %v, want a positive wait", attempt, got)
		}
		if got > limit {
			t.Fatalf("backoff(%d) = %v, want at most the cap %v", attempt, got, limit)
		}
		if attempt <= 5 && got < previous/2 {
			t.Errorf("backoff(%d) = %v, dropped too far below the previous %v", attempt, got, previous)
		}
		previous = got
	}
}

func TestBackoffIsJittered(t *testing.T) {
	t.Parallel()

	seen := make(map[time.Duration]struct{})
	for range 50 {
		seen[backoff(6, time.Second, time.Hour)] = struct{}{}
	}
	if len(seen) < 5 {
		t.Errorf("got %d distinct waits across 50 calls, want jitter", len(seen))
	}
}
