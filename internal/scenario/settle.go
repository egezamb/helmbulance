package scenario

import (
	"context"
	"time"
)

// Settle timings. Variables, not constants, so tests can shorten them.
var (
	settleTimeout = 90 * time.Second // how long problems may take to clear
	settleWindow  = 30 * time.Second // how long it must then stay clean
	settlePoll    = 10 * time.Second
)

// Settle calls check until it has returned no problems for settleWindow, and returns nil, or
// until settleTimeout has passed with problems, and returns them. helm --wait returns as soon as
// pods are ready, so a pod that crashes seconds later is only seen by watching a while longer.
func Settle(ctx context.Context, check func() []string) []string {
	deadline := time.Now().Add(settleTimeout)
	var cleanSince time.Time
	for {
		problems := check()
		now := time.Now()
		switch {
		case len(problems) > 0:
			cleanSince = time.Time{}
			if now.After(deadline) {
				return problems
			}
		case cleanSince.IsZero():
			cleanSince = now
		case now.Sub(cleanSince) >= settleWindow:
			return nil
		}
		select {
		case <-ctx.Done():
			return problems
		case <-time.After(settlePoll):
		}
	}
}
