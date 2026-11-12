package provision

import (
	"testing"
	"time"
)

// runningHost is a host running since t0 at $0.015 an hour with $0.073/GB-month disks:
// with a $1 cap it reaches 80% and 100% at known hours.
func hostAt(t *testing.T, capUSD float64) (*HostBook, *time.Time) {
	b, now := newBook(t, "")
	sp := DefaultSpec("w1", "t3.small")
	sp.BudgetCapUSD = capUSD
	b.Add("w1", "pj_0123456789abcdef", sp, "us-east-1", &Cost{Mode: "spot", ComputeUSDHour: 0.0, IPv4USDHour: 0.0, DiskUSDGBMonth: 0}, Estimate{})
	b.Update("w1", func(h *Host) { h.Rate.ComputeUSDHour = 0.10 }) // $0.10 an hour: $1 lasts 10 h
	b.Observe([]StatusWorker{{Name: "w1", State: "RUNNING", LaunchedAt: tp(t0)}})
	return b, now
}

func kinds(as []Act) string {
	s := ""
	for _, a := range as {
		s += a.Kind + ","
	}
	return s
}

func TestBudgetTimersAt80And100Percent(t *testing.T) {
	b, now := hostAt(t, 1)
	look := func(at time.Duration) []Act {
		*now = t0.Add(at)
		b.Observe([]StatusWorker{{Name: "w1", State: "RUNNING", LaunchedAt: tp(t0)}})
		return b.Due()
	}
	if got := kinds(look(7 * time.Hour)); got != "" { // $0.70
		t.Errorf("at 70%%: %q", got)
	}
	if got := kinds(look(8*time.Hour + time.Minute)); got != "warn80," { // $0.8017
		t.Errorf("at 80%%: %q", got)
	}
	b.MarkWarned("w1")
	if got := kinds(look(9 * time.Hour)); got != "" {
		t.Errorf("the warning repeated: %q", got)
	}
	if got := kinds(look(10*time.Hour + time.Minute)); got != "stop," {
		t.Errorf("at 100%%: %q", got)
	}
	// the stop is recorded; a stopped host over its cap is not stopped again, and never destroyed for the cap
	b.MarkStopped("w1", true)
	*now = t0.Add(11 * time.Hour)
	if got := kinds(b.Due()); got != "" {
		t.Errorf("after the stop: %q", got)
	}
	h, _ := b.Get("w1")
	if h.CapStopped == nil || h.State != "STOPPED" {
		t.Errorf("record: %+v", h)
	}
	// started again by hand, still over the cap: stopped at the next look
	*now = t0.Add(12 * time.Hour)
	b.Observe([]StatusWorker{{Name: "w1", State: "RUNNING", LaunchedAt: tp(t0.Add(11*time.Hour + 30*time.Minute))}})
	if got := kinds(b.Due()); got != "stop," {
		t.Errorf("restarted over the cap: %q", got)
	}
}

func TestTheCloudsTagCanLowerTheCapButNeverRaiseIt(t *testing.T) {
	b, now := hostAt(t, 5)
	*now = t0.Add(6 * time.Hour) // $0.60
	low, high := 0.7, 50.0
	w := StatusWorker{Name: "w1", State: "RUNNING", LaunchedAt: tp(t0)}
	w.Caps.BudgetCapUSD = &low
	b.Observe([]StatusWorker{w})
	if got := kinds(b.Due()); got != "warn80," { // 85% of the tag's $0.70
		t.Errorf("lower tag: %q", got)
	}
	h, _ := b.Get("w1")
	if h.Cap() != 0.7 {
		t.Errorf("cap: %v", h.Cap())
	}
	w.Caps.BudgetCapUSD = &high
	b.Observe([]StatusWorker{w})
	if h, _ := b.Get("w1"); h.Cap() != 5 {
		t.Errorf("a raised tag raised the cap: %v", h.Cap())
	}
}

func TestDestroyAfterStoppedFollowsTheCloudsStopTime(t *testing.T) {
	b, now := hostAt(t, 100)
	stopAt := t0.Add(2 * time.Hour)
	*now = t0.Add(3 * time.Hour)
	b.Observe([]StatusWorker{{Name: "w1", State: "STOPPED", LaunchedAt: tp(t0), LastStopAt: tp(stopAt)}})
	if got := kinds(b.Due()); got != "" {
		t.Errorf("just stopped: %q", got)
	}
	*now = stopAt.Add(71 * time.Hour)
	b.Observe([]StatusWorker{{Name: "w1", State: "STOPPED", LaunchedAt: tp(t0), LastStopAt: tp(stopAt)}})
	if got := kinds(b.Due()); got != "" {
		t.Errorf("at 71 h: %q", got)
	}
	v, _ := b.Views()
	if v[0].DestroyDue != tp(stopAt.Add(72*time.Hour)) {
		t.Errorf("due: %q", v[0].DestroyDue)
	}
	*now = stopAt.Add(72*time.Hour + time.Minute)
	acts := b.Due()
	if kinds(acts) != "destroy," || acts[0].Name != "w1" {
		t.Fatalf("at 72 h: %+v", acts)
	}
	// a failed try holds the next one back
	b.MarkAutoFailed("w1")
	if got := kinds(b.Due()); got != "" {
		t.Errorf("right after a failure: %q", got)
	}
	*now = now.Add(AutoRetryGap + time.Minute)
	b.Observe([]StatusWorker{{Name: "w1", State: "STOPPED", LaunchedAt: tp(t0), LastStopAt: tp(stopAt)}})
	if got := kinds(b.Due()); got != "destroy," {
		t.Errorf("after the gap: %q", got)
	}
	// a running host, a host being destroyed, and a destroyed host are never due
	b.BeginDestroy("w1", "pj_1111111111111111")
	if got := kinds(b.Due()); got != "" {
		t.Errorf("while destroying: %q", got)
	}
}

func TestAStoppedHostNeverDestroysOnABudgetCap(t *testing.T) {
	b, now := hostAt(t, 1)
	*now = t0.Add(10*time.Hour + time.Minute)
	b.Observe([]StatusWorker{{Name: "w1", State: "RUNNING", LaunchedAt: tp(t0)}})
	for _, a := range b.Due() {
		if a.Kind == ActDestroy {
			t.Fatalf("a cap asked for a destroy: %+v", a)
		}
	}
}
