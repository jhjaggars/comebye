package main

import (
	"context"
	"strconv"
	"time"
)

// `worker status --json [NAME]` is the cloud's view of the workers, read-only
// and fast: it asks the cloud API only (no ssh, no hub, no tailnet).
//
//	{"v":1,"provider":"aws","workers":[{"name":"w1","state":"RUNNING","type":"t3.xlarge",
//	  "mode":"spot","launched_at":"...","last_stop_at":null,"caps":{...}}]}
//
// last_stop_at is known while an AWS instance is stopped (the cloud keeps it
// in the transition reason) and null otherwise; the provisioner's own ledger
// carries the history. The caps and the launch price come from the instance's
// tags.

type statusCaps struct {
	BudgetCapUSD      *float64 `json:"budget_cap_usd"`
	DestroyAfterHours *int     `json:"destroy_after_hours"`
	LaunchPriceUSDHr  *float64 `json:"launch_price_usd_hour"`
}

type statusWorker struct {
	Name       string     `json:"name"`
	ID         string     `json:"id,omitempty"`
	State      string     `json:"state"`
	Type       string     `json:"type"`
	Mode       string     `json:"mode,omitempty"`
	Managed    bool       `json:"managed"`
	LaunchedAt *string    `json:"launched_at"`
	LastStopAt *string    `json:"last_stop_at"`
	CreatedAt  *string    `json:"created_at"`
	Caps       statusCaps `json:"caps"`
	Note       string     `json:"note,omitempty"`
}

type statusDoc struct {
	V        int            `json:"v"`
	Provider string         `json:"provider"`
	Workers  []statusWorker `json:"workers"`
}

func tsPtr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func workerStatus(i Inst) statusWorker {
	w := statusWorker{Name: i.Name, ID: i.ID, State: i.State, Type: i.Type, Mode: i.Mode, Managed: i.Managed,
		LaunchedAt: tsPtr(i.Launched), LastStopAt: tsPtr(i.Stopped), CreatedAt: tsPtr(i.Created), Note: i.Note}
	if v, err := strconv.ParseFloat(i.Tags[tagBudgetCap], 64); err == nil {
		w.Caps.BudgetCapUSD = &v
	}
	if v, err := strconv.Atoi(i.Tags[tagDestroyAfter]); err == nil {
		w.Caps.DestroyAfterHours = &v
	}
	if v, err := strconv.ParseFloat(i.Tags[tagLaunchPrice], 64); err == nil {
		w.Caps.LaunchPriceUSDHr = &v
	}
	return w
}

func (e *env) statusJSON(ctx context.Context, name string) error {
	pv := e.prov()
	d := statusDoc{V: 1, Provider: pv.Kind(), Workers: []statusWorker{}}
	if name != "" {
		if err := checkName(name); err != nil {
			return err
		}
		inst, err := pv.Describe(ctx, name)
		if err != nil {
			return err
		}
		if inst != nil {
			d.Workers = append(d.Workers, workerStatus(*inst))
		}
	} else {
		var all []Inst
		var err error
		if fl, ok := pv.(fullLister); ok {
			all, err = fl.AllInst(ctx)
		} else if all, err = pv.Instances(ctx); err == nil {
			for k := range all {
				if inst, derr := pv.Describe(ctx, all[k].Name); derr == nil && inst != nil {
					all[k] = *inst
				}
			}
		}
		if err != nil {
			return err
		}
		for _, i := range all {
			d.Workers = append(d.Workers, workerStatus(i))
		}
	}
	b := marshalJSON(d)
	e.printf("%s\n", b)
	return nil
}
