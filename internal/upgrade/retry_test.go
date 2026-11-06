package upgrade

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func retryOpts() (*Options, *[]time.Duration, *bytes.Buffer) {
	var slept []time.Duration
	var out bytes.Buffer
	return &Options{Out: &out, Err: &out, Sleep: func(d time.Duration) { slept = append(slept, d) }}, &slept, &out
}

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	o, slept, out := retryOpts()
	calls := 0
	err := retry(o, "agents", retryDelays, func() error {
		calls++
		if calls <= 3 {
			return errors.New("node x is not a registered peer of this hub")
		}
		return nil
	})
	if err != nil || calls != 4 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if len(*slept) != 3 || (*slept)[0] != 2*time.Second || (*slept)[2] != 8*time.Second {
		t.Fatalf("slept %v", *slept)
	}
	if n := strings.Count(out.String(), "attempt "); n != 3 || !strings.Contains(out.String(), "attempt 1/5 failed (node x is not a registered peer") {
		t.Fatalf("log:\n%s", out.String())
	}
}

func TestRetryGivesUpWithLastError(t *testing.T) {
	o, slept, _ := retryOpts()
	calls := 0
	err := retry(o, "agents", retryDelays, func() error {
		calls++
		return errors.New("attempt-error " + string(rune('0'+calls)))
	})
	if calls != 5 || err == nil || err.Error() != "attempt-error 5" {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	if len(*slept) != 4 {
		t.Fatalf("slept %v", *slept)
	}
}

func TestRetryStopsAtOnceOnFatal(t *testing.T) {
	o, slept, out := retryOpts()
	calls := 0
	err := retry(o, "served version", retryDelays, func() error {
		calls++
		return fatal(errors.New("wrong version"))
	})
	if calls != 1 || err == nil || err.Error() != "wrong version" || len(*slept) != 0 || out.Len() != 0 {
		t.Fatalf("calls=%d err=%v slept=%v out=%q", calls, err, *slept, out.String())
	}
}

func TestTransientAgents(t *testing.T) {
	for s, want := range map[string]bool{
		`node h is not a registered peer of this hub`: true,
		`not_a_peer`:                                            true,
		`the hub (x) is unreachable`:                            true,
		`dial tcp: connection refused`:                          true,
		`node h no longer carries the tag tag:w its peer`:       false,
		`the hub rejected this peer; the hub's -ts-allow-login`: false,
		`unknown flag`:                                          false,
	} {
		if got := transientAgents(s); got != want {
			t.Errorf("transientAgents(%q) = %v, want %v", s, got, want)
		}
	}
}
