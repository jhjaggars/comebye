package main

import (
	"context"
	"fmt"
	"strings"
)

// Provider is the cloud half of a worker: networks, disks, the VM and the
// way its ssh host key is vouched for. Enrolment, bootstrap, the hub join
// and the order of destroy are shared and live in commands.go.
type Provider interface {
	Kind() string
	// Describe returns nil, nil when there is no such (live) instance.
	Describe(ctx context.Context, name string) (*Inst, error)
	EnsureNetwork(ctx context.Context, w logf) error
	EnsureDataDisk(ctx context.Context, name string, w logf) error
	// Create makes the instance (it must not exist). pubKey is our public ssh key.
	Create(ctx context.Context, name, pubKey string) error
	// Attach makes sure the data disk is attached to the instance (idempotent).
	Attach(ctx context.Context, name string) error
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	// Delete removes the instance (not the data disk) and waits for it.
	Delete(ctx context.Context, name string) error
	// DeleteDataDisk deletes the data disk; absent is fine.
	DeleteDataDisk(ctx context.Context, name string) error
	// HostKey returns the VM's ssh-ed25519 host key as the cloud API vouches
	// for it, or "" when it is not published yet.
	HostKey(ctx context.Context, name string, inst *Inst) (string, error)
	// List prints every instance and disk the tool created.
	List(ctx context.Context, w logf) error
	// CleanupNetwork removes the tool-created network pieces, only if every
	// one is marked managed-by=comebye-worker and nothing else uses them.
	CleanupNetwork(ctx context.Context, w logf) error
	// Plan prints what Create would run, without running anything.
	Plan(name, pubKey string, w logf)
}

type logf func(format string, a ...any)

const (
	stateRunning = "RUNNING"
	stateStopped = "STOPPED"
)

// Inst is an instance as the commands see it, whatever the cloud.
type Inst struct {
	Name, ID string
	State    string // RUNNING, STOPPED, or the cloud's own word
	Type     string
	IP       string // external IPv4
	Managed  bool   // carries managed-by=comebye-worker
}

func newProvider(r Runner, c *Config) Provider {
	if c.Provider == "aws" {
		return &awsProv{a: &awsCLI{r: r, cfg: c}, cfg: c}
	}
	return &gcloud{r: r, cfg: c}
}

func (e *env) prov() Provider {
	if e.P == nil {
		e.P = newProvider(e.Run, e.Cfg)
	}
	return e.P
}

// checkOwned refuses to touch an instance this tool did not label.
func checkOwned(i *Inst) error {
	if !i.Managed {
		return fmt.Errorf("instance %s is not labelled %s=%s: refusing to manage it", i.Name, labelKey, labelVal)
	}
	return nil
}

// cmdLine renders an argument list as a copy-pasteable command line.
func cmdLine(bin string, args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if shellSafe.MatchString(a) {
			q[i] = a
		} else {
			q[i] = shellQuote(a)
		}
	}
	return bin + " " + strings.Join(q, " ")
}

// managedDesc is the marker on GCE network resources, which cannot carry
// labels: it goes in their description.
const managedDesc = labelKey + "=" + labelVal
