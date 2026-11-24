package natsutil

import (
	"testing"
	"time"
)

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		attempt uint64
		want    time.Duration
	}{
		{0, 5 * time.Second}, // no metadata: treat as the first attempt
		{1, 5 * time.Second},
		{2, 15 * time.Second},
		{3, 45 * time.Second},
		{4, 2 * time.Minute}, // 135s, capped
		{5, 2 * time.Minute},
		{1000, 2 * time.Minute},
	}
	for _, c := range cases {
		if got := RetryDelay(c.attempt); got != c.want {
			t.Errorf("RetryDelay(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}
