package provision

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The hosts a provisioner has built, and what each has cost.
//
// A Host is the provisioner's own record of one host it made (written when the
// instance exists, kept after destroy for the final bill). Its spend is not
// read from the cloud's bill, which lags a day and costs a call each time: it
// is the running intervals the cloud reports (launch time, last stop time),
// times the price recorded at launch, plus the public IPv4 while it runs, plus
// the disks from creation to destroy. It is an estimate, and the app says
// "about".

// Interval is one stretch of running: from a launch (or a start) to a stop.
type Interval struct {
	Start time.Time  `json:"start"`
	End   *time.Time `json:"end,omitempty"`
}

// Rate is what an hour of a host costs, as priced when it was created. The
// compute part is replaced by the launch price the cloud's own tag records.
type Rate struct {
	ComputeUSDHour float64 `json:"compute_usd_hour"`
	FromTag        bool    `json:"from_tag,omitempty"`
	IPv4USDHour    float64 `json:"ipv4_usd_hour"`
	DiskUSDGBMonth float64 `json:"disk_usd_gb_month"`
}

// Host is one host a provisioner built.
type Host struct {
	Name      string     `json:"name"`
	JobID     string     `json:"job_id,omitempty"`
	Spec      Spec       `json:"spec"`
	Region    string     `json:"region,omitempty"`
	Mode      string     `json:"mode,omitempty"` // spot or on_demand
	Rate      Rate       `json:"rate"`
	Created   time.Time  `json:"created"`
	State     string     `json:"state"` // the cloud's: PENDING RUNNING STOPPING STOPPED; GONE when it no longer exists
	Intervals []Interval `json:"intervals"`
	LastSeen  time.Time  `json:"last_seen"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"` // when it last stopped (idle, cap or reclaim)

	Destroying string     `json:"destroying,omitempty"` // the job id while a destroy runs
	Destroyed  *time.Time `json:"destroyed,omitempty"`
	DataKept   bool       `json:"data_disk_kept,omitempty"`
	Final      *float64   `json:"final_usd,omitempty"`

	// Safety timers (slice 4).
	Warned80      bool       `json:"warned_80,omitempty"`      // the 80% to-do was raised
	CapStopped    *time.Time `json:"cap_stopped,omitempty"`    // the watchdog stopped it at the budget cap
	AutoDestroy   string     `json:"auto_destroy,omitempty"`   // why a destroy was started by a timer
	DestroyFailed string     `json:"destroy_failed,omitempty"` // the last failed destroy, one line
}

// Spend is what a host has cost so far, or in all.
type Spend struct {
	USD      float64 `json:"usd"`
	Compute  float64 `json:"compute_usd"`
	Disks    float64 `json:"disks_usd"`
	Hours    float64 `json:"running_hours"`
	USDHour  float64 `json:"usd_hour"` // what it costs per hour right now (0 when stopped or gone)
	Final    bool    `json:"final"`
	Estimate bool    `json:"estimate"`
}

func hours(a, b time.Time) float64 {
	if b.Before(a) {
		return 0
	}
	return b.Sub(a).Hours()
}

func (h *Host) running() bool {
	return h.State == "PENDING" || h.State == "RUNNING" || h.State == "STOPPING"
}

func (h *Host) endOfLife(now time.Time) time.Time {
	if h.Destroyed != nil {
		return *h.Destroyed
	}
	return now
}

// Spend prices the host as of now (or of its destroy, once destroyed).
func (h *Host) Spend(now time.Time) Spend {
	end := h.endOfLife(now)
	var sp Spend
	perHour := h.Rate.ComputeUSDHour + h.Rate.IPv4USDHour
	for _, iv := range h.Intervals {
		stop := end
		if iv.End != nil && iv.End.Before(end) {
			stop = *iv.End
		}
		sp.Hours += hours(iv.Start, stop)
	}
	sp.Compute = sp.Hours * perHour
	gb := float64(h.Spec.BootDiskGB + h.Spec.DataDiskGB)
	if h.Destroyed != nil && h.DataKept {
		gb = float64(h.Spec.BootDiskGB) // the data disk goes on costing after destroy: not part of the bill
	}
	sp.Disks = hours(h.Created, end) * gb * h.Rate.DiskUSDGBMonth / hoursPerMonth
	sp.USD = round4(sp.Compute + sp.Disks)
	sp.Compute, sp.Disks, sp.Hours = round4(sp.Compute), round4(sp.Disks), round4(sp.Hours)
	sp.Estimate = true
	if h.Destroyed != nil {
		sp.Final = true
		if h.Final != nil {
			sp.USD = *h.Final
		}
	} else if h.running() {
		sp.USDHour = round4(perHour + gb*h.Rate.DiskUSDGBMonth/hoursPerMonth)
	} else if h.State == "STOPPED" {
		sp.USDHour = round4(gb * h.Rate.DiskUSDGBMonth / hoursPerMonth) // only the disks
	}
	return sp
}

// RunningNow is whether the cloud last said the host is up.
func (h *Host) RunningNow() bool { return h.Destroyed == nil && h.running() }

// ---- what the cloud says ----

// StatusWorker is one instance of `worker status --json`.
type StatusWorker struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Type       string `json:"type"`
	Mode       string `json:"mode"`
	Managed    bool   `json:"managed"`
	LaunchedAt string `json:"launched_at"`
	LastStopAt string `json:"last_stop_at"`
	Caps       struct {
		BudgetCapUSD      *float64 `json:"budget_cap_usd"`
		DestroyAfterHours *int     `json:"destroy_after_hours"`
		LaunchPriceUSDHr  *float64 `json:"launch_price_usd_hour"`
	} `json:"caps"`
}

func (w StatusWorker) launched() *time.Time { return parseT(w.LaunchedAt) }
func (w StatusWorker) stopped() *time.Time  { return parseT(w.LastStopAt) }

func parseT(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

// ParseStatus decodes `status --json`: only the named fields survive.
func ParseStatus(b []byte) ([]StatusWorker, error) {
	var d struct {
		V       int            `json:"v"`
		Workers []StatusWorker `json:"workers"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("the status is not understood: %w", err)
	}
	if d.V != 1 {
		return nil, fmt.Errorf("the status is version %d, not 1", d.V)
	}
	var out []StatusWorker
	for _, w := range d.Workers {
		if !nameRe.MatchString(w.Name) {
			continue
		}
		switch w.State {
		case "PENDING", "RUNNING", "STOPPING", "STOPPED":
		default:
			w.State = "UNKNOWN"
		}
		if len(out) < 200 {
			out = append(out, w)
		}
	}
	return out, nil
}

// ---- the book ----

// HostBook is the provisioner's record of its hosts, in a file.
type HostBook struct {
	mu    sync.Mutex
	path  string
	hosts map[string]*Host
	Now   func() time.Time
}

const maxHosts = 100

var errNoHost = errors.New("no such host here")

// OpenHostBook reads the book in dir (empty dir: kept in memory only).
func OpenHostBook(dir string) *HostBook {
	b := &HostBook{hosts: map[string]*Host{}, Now: time.Now}
	if dir == "" {
		return b
	}
	if err := os.MkdirAll(filepath.Join(dir, "provision"), 0o700); err != nil {
		return b
	}
	b.path = filepath.Join(dir, "provision", "hosts.json")
	raw, err := os.ReadFile(b.path)
	if err != nil {
		return b
	}
	var list []*Host
	if json.Unmarshal(raw, &list) != nil {
		return b
	}
	for _, h := range list {
		if nameRe.MatchString(h.Name) && h.Spec.Validate() == nil {
			b.hosts[h.Name] = h
		}
	}
	return b
}

func (b *HostBook) saveLocked() {
	if b.path == "" {
		return
	}
	list := make([]*Host, 0, len(b.hosts))
	for _, h := range b.hosts {
		list = append(list, h)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
	if len(list) > maxHosts { // forget the oldest destroyed ones
		var keep []*Host
		drop := len(list) - maxHosts
		for _, h := range list {
			if drop > 0 && h.Destroyed != nil {
				delete(b.hosts, h.Name)
				drop--
				continue
			}
			keep = append(keep, h)
		}
		list = keep
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return
	}
	tmp := b.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		os.Rename(tmp, b.path)
	}
}

// Add records a host whose instance exists. An old record of a destroyed host
// of the same name is replaced; a live one is kept (a retry of the same job).
func (b *HostBook) Add(name, jobID string, spec Spec, region string, cost *Cost, est Estimate) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if h := b.hosts[name]; h != nil && h.Destroyed == nil {
		return
	}
	now := b.Now()
	h := &Host{Name: name, JobID: jobID, Spec: spec, Region: region, Created: now, State: "PENDING", LastSeen: now}
	h.Mode = "on_demand"
	if spec.Spot {
		h.Mode = "spot"
	}
	if cost != nil {
		h.Mode = cost.Mode
		h.Rate.ComputeUSDHour = cost.ComputeUSDHour
		h.Rate.IPv4USDHour = cost.IPv4USDHour
		h.Rate.DiskUSDGBMonth = cost.DiskUSDGBMonth
	} else {
		h.Rate.IPv4USDHour, h.Rate.DiskUSDGBMonth = est.IPv4USDHour, est.DiskUSDGBMonth
	}
	b.hosts[name] = h
	b.saveLocked()
}

// Observe folds what the cloud says into the records. A host the cloud no
// longer lists is closed as gone. Hosts the book does not know are ignored.
func (b *HostBook) Observe(ws []StatusWorker) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.Now()
	seen := map[string]StatusWorker{}
	for _, w := range ws {
		seen[w.Name] = w
	}
	for name, h := range b.hosts {
		if h.Destroyed != nil {
			continue
		}
		w, ok := seen[name]
		if !ok {
			if h.State != "GONE" && now.Sub(h.Created) > 10*time.Minute { // not yet listed right after the launch
				closeOpen(h, now)
				h.State = "GONE"
			}
			continue
		}
		h.apply(w, now)
	}
	b.saveLocked()
}

func closeOpen(h *Host, at time.Time) {
	if n := len(h.Intervals); n > 0 && h.Intervals[n-1].End == nil {
		if at.Before(h.Intervals[n-1].Start) {
			at = h.Intervals[n-1].Start
		}
		h.Intervals[n-1].End = &at
	}
}

func (h *Host) apply(w StatusWorker, now time.Time) {
	prev := h.State
	h.State = w.State
	if w.Mode != "" {
		h.Mode = map[string]string{"spot": "spot", "on-demand": "on_demand"}[w.Mode]
		if h.Mode == "" {
			h.Mode = "on_demand"
		}
	}
	if p := w.Caps.LaunchPriceUSDHr; p != nil && *p > 0 && *p < 1000 {
		h.Rate.ComputeUSDHour, h.Rate.FromTag = *p, true
	}
	switch w.State {
	case "PENDING", "RUNNING", "STOPPING":
		launch := w.launched()
		if launch == nil {
			launch = &now
		}
		n := len(h.Intervals)
		switch {
		case n > 0 && h.Intervals[n-1].End == nil && h.Intervals[n-1].Start.Equal(*launch):
		case n > 0 && h.Intervals[n-1].End == nil:
			// started again between two looks: the old run ended when last seen
			closeOpen(h, h.LastSeen)
			h.Intervals = append(h.Intervals, Interval{Start: *launch})
		default:
			h.Intervals = append(h.Intervals, Interval{Start: *launch})
		}
		if prev == "STOPPED" {
			h.StoppedAt = nil
		}
	case "STOPPED":
		at := now
		if s := w.stopped(); s != nil {
			at = *s
		}
		closeOpen(h, at)
		if h.StoppedAt == nil {
			h.StoppedAt = &at
		}
	}
	h.LastSeen = now
}

// MarkStopped records a stop the provisioner itself made (the cap).
func (b *HostBook) MarkStopped(name string, capStop bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.hosts[name]
	if h == nil || h.Destroyed != nil {
		return
	}
	now := b.Now()
	closeOpen(h, now)
	h.State, h.StoppedAt = "STOPPED", &now
	if capStop {
		h.CapStopped = &now
	}
	b.saveLocked()
}

// Get returns a copy of one record.
func (b *HostBook) Get(name string) (Host, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.hosts[name]
	if h == nil {
		return Host{}, false
	}
	return *h, true
}

// Update changes one record under the lock.
func (b *HostBook) Update(name string, f func(*Host)) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.hosts[name]
	if h == nil {
		return false
	}
	f(h)
	b.saveLocked()
	return true
}

// BeginDestroy marks a destroy as running.
func (b *HostBook) BeginDestroy(name, job string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.hosts[name]
	switch {
	case h == nil:
		return errNoHost
	case h.Destroyed != nil:
		return errors.New("this host is already destroyed")
	}
	h.Destroying, h.DestroyFailed = job, ""
	b.saveLocked()
	return nil
}

// EndDestroy closes a destroy: on success the host is destroyed and its bill
// is final; on failure it stays, with the reason.
func (b *HostBook) EndDestroy(name string, ok bool, dataKept bool, why string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.hosts[name]
	if h == nil {
		return
	}
	h.Destroying = ""
	if !ok {
		h.DestroyFailed = Text(why, 200)
		b.saveLocked()
		return
	}
	now := b.Now()
	closeOpen(h, now)
	h.Destroyed, h.DataKept, h.DestroyFailed = &now, dataKept, ""
	h.State = "GONE"
	sp := h.Spend(now)
	f := sp.USD
	h.Final = &f
	b.saveLocked()
}

// HostView is a host as the app reads it.
type HostView struct {
	Name          string  `json:"name"`
	State         string  `json:"state"` // running, stopped, starting, stopping, destroying, destroyed, gone, unknown
	Type          string  `json:"type"`
	Region        string  `json:"region,omitempty"`
	Mode          string  `json:"mode"`
	Created       string  `json:"created"`
	Destroyed     string  `json:"destroyed,omitempty"`
	StoppedAt     string  `json:"stopped_at,omitempty"`
	Spend         Spend   `json:"spend"`
	BudgetCapUSD  float64 `json:"budget_cap_usd"`
	DestroyAfterH int     `json:"destroy_after_hours"`
	IdleMinutes   int     `json:"idle_minutes"`
	MaxRun        string  `json:"max_run,omitempty"`
	DataDiskGB    int     `json:"data_disk_gb"`
	DataKept      bool    `json:"data_disk_kept,omitempty"`
	DestroyJob    string  `json:"destroy_job,omitempty"`
	DestroyFailed string  `json:"destroy_failed,omitempty"`
	CapStopped    string  `json:"cap_stopped,omitempty"`
	AutoDestroy   string  `json:"auto_destroy,omitempty"`
	DestroyDue    string  `json:"destroy_due,omitempty"` // when a stopped host will be destroyed
	Warned80      bool    `json:"warned_80,omitempty"`
	FromTag       bool    `json:"price_from_cloud"`
	DiskUSDGBMon  float64 `json:"disk_usd_gb_month"`
	AsOf          string  `json:"as_of"`
}

func rfc(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (h *Host) view(now time.Time) HostView {
	v := HostView{Name: h.Name, Type: h.Spec.Type, Region: h.Region, Mode: h.Mode, Created: h.Created.UTC().Format(time.RFC3339),
		Destroyed: rfc(h.Destroyed), StoppedAt: rfc(h.StoppedAt), Spend: h.Spend(now), BudgetCapUSD: h.Spec.BudgetCapUSD,
		DestroyAfterH: h.Spec.DestroyAfterHours, IdleMinutes: h.Spec.IdleMinutes, MaxRun: h.Spec.MaxRun, DataDiskGB: h.Spec.DataDiskGB,
		DataKept: h.DataKept, DestroyJob: h.Destroying, DestroyFailed: h.DestroyFailed, CapStopped: rfc(h.CapStopped),
		AutoDestroy: h.AutoDestroy, Warned80: h.Warned80, FromTag: h.Rate.FromTag, DiskUSDGBMon: h.Rate.DiskUSDGBMonth, AsOf: now.UTC().Format(time.RFC3339)}
	switch {
	case h.Destroyed != nil:
		v.State = "destroyed"
	case h.Destroying != "":
		v.State = "destroying"
	default:
		v.State = map[string]string{"PENDING": "starting", "RUNNING": "running", "STOPPING": "stopping", "STOPPED": "stopped", "GONE": "gone"}[h.State]
		if v.State == "" {
			v.State = "unknown"
		}
	}
	if h.State == "STOPPED" && h.Destroyed == nil && h.StoppedAt != nil && h.Spec.DestroyAfterHours > 0 {
		due := h.StoppedAt.Add(time.Duration(h.Spec.DestroyAfterHours) * time.Hour)
		v.DestroyDue = due.UTC().Format(time.RFC3339)
	}
	return v
}

// Views lists the hosts, newest first, with the total of what they cost.
func (b *HostBook) Views() ([]HostView, Total) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.Now()
	var hs []*Host
	for _, h := range b.hosts {
		hs = append(hs, h)
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i].Created.After(hs[j].Created) })
	out := make([]HostView, 0, len(hs))
	var t Total
	for _, h := range hs {
		v := h.view(now)
		out = append(out, v)
		t.USD += v.Spend.USD
		t.USDHour += v.Spend.USDHour
		if h.Destroyed == nil {
			t.Live++
		}
	}
	t.USD, t.USDHour = round4(t.USD), round4(t.USDHour)
	return out, t
}

// Total is what all the hosts have cost, and burn now.
type Total struct {
	USD     float64 `json:"usd"`
	USDHour float64 `json:"usd_hour"`
	Live    int     `json:"live"`
}

// Live lists copies of the hosts not yet destroyed.
func (b *HostBook) Live() []Host {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Host
	for _, h := range b.hosts {
		if h.Destroyed == nil {
			out = append(out, *h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// HostsView is the hub's reading of a node's hosts.
type HostsView struct {
	V     int        `json:"v"`
	Hosts []HostView `json:"hosts"`
	Total Total      `json:"total"`
}

var rfcRe = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)

func cleanTime(s string) string {
	if rfcRe.MatchString(s) {
		return s
	}
	return ""
}

func finite(f float64, max float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > max {
		return 0
	}
	return f
}

// ParseHosts decodes a node's hosts answer. Only the named fields survive,
// every string is scrubbed and every number is bounded: the hub shows what a
// peer says, so it believes none of it.
func ParseHosts(b []byte) (*HostsView, error) {
	var v HostsView
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("the hosts are not understood: %w", err)
	}
	if v.V != 1 {
		return nil, fmt.Errorf("the hosts are version %d, not 1", v.V)
	}
	var out []HostView
	for _, h := range v.Hosts {
		if !nameRe.MatchString(h.Name) || len(out) >= maxHosts {
			continue
		}
		switch h.State {
		case "running", "stopped", "starting", "stopping", "destroying", "destroyed", "gone", "unknown":
		default:
			h.State = "unknown"
		}
		if !typeRe.MatchString(h.Type) {
			h.Type = ""
		}
		if !regionRe.MatchString(h.Region) {
			h.Region = ""
		}
		if h.Mode != "spot" {
			h.Mode = "on_demand"
		}
		h.Created, h.Destroyed, h.StoppedAt, h.CapStopped, h.DestroyDue, h.AsOf = cleanTime(h.Created), cleanTime(h.Destroyed), cleanTime(h.StoppedAt), cleanTime(h.CapStopped), cleanTime(h.DestroyDue), cleanTime(h.AsOf)
		sp := &h.Spend
		sp.USD, sp.Compute, sp.Disks, sp.Hours, sp.USDHour = finite(sp.USD, 1e7), finite(sp.Compute, 1e7), finite(sp.Disks, 1e7), finite(sp.Hours, 1e7), finite(sp.USDHour, 1e4)
		h.BudgetCapUSD, h.DiskUSDGBMon = finite(h.BudgetCapUSD, 1e4), finite(h.DiskUSDGBMon, 10)
		h.DestroyAfterH, h.IdleMinutes, h.DataDiskGB = clampInt(h.DestroyAfterH, 0, 100000), clampInt(h.IdleMinutes, 0, 100000), clampInt(h.DataDiskGB, 0, 100000)
		h.MaxRun = strings.TrimSpace(Text(h.MaxRun, 8))
		h.DestroyJob = Text(h.DestroyJob, 24)
		h.DestroyFailed, h.AutoDestroy = Text(h.DestroyFailed, 200), Text(h.AutoDestroy, 120)
		out = append(out, h)
	}
	v.Hosts = out
	v.Total.USD, v.Total.USDHour, v.Total.Live = finite(v.Total.USD, 1e7), finite(v.Total.USDHour, 1e4), clampInt(v.Total.Live, 0, 1000)
	return &v, nil
}
