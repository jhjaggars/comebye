package provision

import (
	"fmt"
	"time"
)

// The provisioner's safety timers. They run on the node that built a host (an
// offline hub changes nothing here) and read each host's caps from what the
// cloud's own tags say, falling back to what the host was asked for.
//
//	80% of the budget cap      a to-do (the hub raises it from the host's record)
//	100% of the budget cap     stop the host, disks kept
//	stopped for destroy-after  start it, save its sessions, destroy it
//
// A budget cap never destroys anything: a stop loses no data, only a destroy
// does, and a destroy is the one control that needs a person's node.

// Cap is the budget cap in force: the lower of what the host was asked for and
// what the cloud's tag says (a tag someone raised does not raise the cap).
func (h *Host) Cap() float64 {
	c := h.Spec.BudgetCapUSD
	if h.CapUSD > 0 && (c <= 0 || h.CapUSD < c) {
		c = h.CapUSD
	}
	return c
}

// destroyDue is when a stopped host is destroyed, from the last stop.
func (h *Host) destroyDue() (time.Time, bool) {
	hrs := h.Spec.DestroyAfterHours
	if h.DestroyAfterHrs > 0 && (hrs <= 0 || h.DestroyAfterHrs < hrs) {
		hrs = h.DestroyAfterHrs
	}
	if h.StoppedAt == nil || hrs <= 0 {
		return time.Time{}, false
	}
	return h.StoppedAt.Add(time.Duration(hrs) * time.Hour), true
}

// Action kinds a check can ask for.
const (
	ActWarn    = "warn80"  // record the 80% warning (the hub turns it into a to-do)
	ActStop    = "stop"    // stop the host: it reached its cap
	ActDestroy = "destroy" // destroy a host that has been stopped long enough
)

// Act is one thing the watchdog should do for a host.
type Act struct {
	Name string
	Kind string
	Why  string
}

// Check says what is due for a host at now. It changes nothing.
func (h *Host) Check(now time.Time) []Act {
	if h.Destroyed != nil || h.Destroying != "" {
		return nil
	}
	var out []Act
	capUSD := h.Cap()
	sp := h.Spend(now)
	if capUSD > 0 {
		if sp.USD >= capUSD && h.RunningNow() {
			out = append(out, Act{h.Name, ActStop, fmt.Sprintf("spent about $%.2f of its $%.2f budget cap", sp.USD, capUSD)})
		}
		if sp.USD >= 0.8*capUSD && !h.Warned80 {
			out = append(out, Act{h.Name, ActWarn, fmt.Sprintf("about $%.2f of its $%.2f budget cap is spent", sp.USD, capUSD)})
		}
	}
	if due, ok := h.destroyDue(); ok && h.State == "STOPPED" && !now.Before(due) && !now.Before(h.NextAuto) {
		out = append(out, Act{h.Name, ActDestroy, fmt.Sprintf("stopped since %s, more than the %s it was asked to wait", h.StoppedAt.UTC().Format("2006-01-02 15:04"), due.Sub(*h.StoppedAt).Round(time.Hour))})
	}
	return out
}

// Due lists what is due across the live hosts, for the watchdog.
func (b *HostBook) Due() []Act {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.Now()
	var out []Act
	for _, h := range b.hosts {
		out = append(out, h.Check(now)...)
	}
	return out
}

// MarkWarned records the 80% warning once.
func (b *HostBook) MarkWarned(name string) {
	b.Update(name, func(h *Host) {
		if !h.Warned80 {
			now := b.Now()
			h.Warned80, h.Warned80At = true, &now
		}
	})
}

// AutoRetryGap is how long after a failed timer destroy the next try waits: a
// try starts the host, which costs, so it is not repeated every few minutes.
const AutoRetryGap = 6 * time.Hour

// MarkAutoDestroy records that a timer started a destroy, and why.
func (b *HostBook) MarkAutoDestroy(name, why string) {
	b.Update(name, func(h *Host) { h.AutoDestroy = Text(why, 120) })
}

// MarkAutoFailed holds the next timer destroy back after a failed one.
func (b *HostBook) MarkAutoFailed(name string) {
	b.Update(name, func(h *Host) { h.NextAuto = b.Now().Add(AutoRetryGap) })
}
