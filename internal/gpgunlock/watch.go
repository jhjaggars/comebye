package gpgunlock

import (
	"context"
	"time"
)

// Run enforces the lifetime until ctx ends: it sweeps at start (a restart
// after the expiry forgets at once), then at each expiry, and at least every
// minute so an unlock made by another process is noticed. Errors go to log.
func (h *Host) Run(ctx context.Context, log func(error)) {
	for {
		if _, err := h.Sweep(); err != nil && log != nil {
			log(err)
		}
		wait := time.Minute
		if at, ok := h.NextExpiry(); ok {
			if d := at.Sub(h.now()); d < wait {
				wait = d
			}
		}
		if wait < 50*time.Millisecond {
			wait = 50 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
