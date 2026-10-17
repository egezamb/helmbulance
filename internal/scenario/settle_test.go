package scenario

import (
	"context"
	"testing"
	"time"
)

// shortSettle shrinks the settle timings for a test.
func shortSettle(t *testing.T, timeout, window time.Duration) {
	t.Helper()
	to, w, p := settleTimeout, settleWindow, settlePoll
	t.Cleanup(func() { settleTimeout, settleWindow, settlePoll = to, w, p })
	settleTimeout, settleWindow, settlePoll = timeout, window, time.Millisecond
}

// reads returns a check that gives the next answer on each call, then repeats the last.
func reads(answers ...[]string) func() []string {
	i := 0
	return func() []string {
		a := answers[min(i, len(answers)-1)]
		i++
		return a
	}
}

// A pod that crashes after the first clean read is seen.
func TestSettleSeesAProblemAfterAFirstCleanRead(t *testing.T) {
	shortSettle(t, 20*time.Millisecond, time.Second)
	got := Settle(t.Context(), reads(nil, nil, []string{"pod `web` restarted"}))
	if len(got) != 1 {
		t.Errorf("Settle = %q, want the restart", got)
	}
}

func TestSettleCleanForTheWindow(t *testing.T) {
	shortSettle(t, time.Second, 10*time.Millisecond)
	if got := Settle(t.Context(), reads([]string{"pod `web` is not ready"}, nil)); got != nil {
		t.Errorf("Settle = %q, want nil once ready and clean for the window", got)
	}
}

func TestSettleCancelled(t *testing.T) {
	shortSettle(t, time.Hour, time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := Settle(ctx, reads([]string{"x"})); len(got) != 1 {
		t.Errorf("Settle after cancel = %q, want the last problems", got)
	}
}
