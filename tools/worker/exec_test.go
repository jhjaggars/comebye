package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A cancelled command ends with the children it started: ssh's ProxyCommand
// (virtctl port-forward) must not be left running.
func TestRunKillsTheProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	// the shell starts a child that holds the pipe open, like a ProxyCommand
	_, _, err := osRunner{}.Run(ctx, "sh", []string{"-c", "sleep 30 & echo $! >&2; wait"}, nil)
	if err == nil || time.Since(start) > 8*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	pid := strings.TrimSpace(strings.Split(err.Error(), ": ")[len(strings.Split(err.Error(), ": "))-1])
	time.Sleep(200 * time.Millisecond)
	if out, _ := exec.Command("sh", "-c", "kill -0 "+pid+" 2>&1 && echo alive").Output(); strings.Contains(string(out), "alive") {
		t.Errorf("child %s survived the cancel", pid)
	}
}
