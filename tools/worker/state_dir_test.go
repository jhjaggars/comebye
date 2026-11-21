package main

import "testing"

// In a pod the worker directory (each worker's ssh key and pinned host key)
// sits under XDG_STATE_HOME, which the provisioner manifests point at the
// claim; state_dir in the config says the same outright.
func TestStateDirFollowsXDGStateHomeAndTheConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/var/lib/pvc")
	if got := defaults().StateDir; got != "/var/lib/pvc/comebye-worker" {
		t.Errorf("default state dir = %q", got)
	}
	c := defaults()
	if err := c.apply(map[string]string{"state_dir": "/var/lib/comebye-worker"}); err != nil || c.StateDir != "/var/lib/comebye-worker" {
		t.Errorf("state_dir = %q, %v", c.StateDir, err)
	}
}
