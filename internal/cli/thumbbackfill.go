package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// `comebye pages thumbs --backfill`: gallery thumbnails for pages published
// before they worked (or from a host that could not make them). For each
// page's CURRENT version that has none it fetches the stored HTML, renders it
// as publish does, and uploads the pictures for that version. Page content
// and versions are never touched. It acts as the caller does: the user or a
// coordinator reaches every page; any other agent only the pages it may read
// and change (its own, or its host's on a tagged node).

const backfillJobs = 3 // pages rendered at once; each render runs a light and a dark Chrome

type backfillResult struct {
	ID      string
	Version int
	Status  string // stored, skipped, failed
	Why     string
	Took    time.Duration
}

// backfillTargets lists the pages to look at: the ids given, or all.
func backfillTargets(c *client, ids []string) ([]apiPageV, error) {
	if len(ids) == 0 {
		data, err := c.raw("GET", "/api/pages", nil, nil)
		if err != nil {
			return nil, err
		}
		var r struct {
			Pages []apiPageV `json:"pages"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r.Pages, nil
	}
	var out []apiPageV
	for _, id := range ids {
		data, err := c.raw("GET", "/api/pages/"+url.PathEscape(id), nil, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", id, err)
		}
		var r struct {
			Page apiPageV `json:"page"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		out = append(out, r.Page)
	}
	return out, nil
}

// apiPageV is a page with its versions' thumbnail state.
type apiPageV struct {
	apiPage
	Versions []struct {
		N      int      `json:"n"`
		Thumbs []string `json:"thumbs"`
	} `json:"versions"`
}

func (p apiPageV) currentHasThumbs() bool {
	for _, v := range p.Versions {
		if v.N == p.Version {
			return len(v.Thumbs) > 0
		}
	}
	return false
}

func backfillOne(e *Env, c *client, p apiPageV, dry bool) (r backfillResult) {
	r = backfillResult{ID: p.ID, Version: p.Version}
	switch {
	case p.Kind != "html":
		r.Status, r.Why = "skipped", "not HTML ("+p.Kind+")"
		return r
	case p.currentHasThumbs():
		r.Status, r.Why = "skipped", "already has thumbnails"
		return r
	case dry:
		r.Status, r.Why = "skipped", "dry run: would render and upload"
		return r
	}
	start := time.Now()
	defer func() { r.Took = time.Since(start) }()
	data, err := c.raw("GET", "/api/pages/"+url.PathEscape(p.ID), url.Values{"body": {"1"}}, nil)
	if err != nil {
		r.Status, r.Why = "failed", "reading it: "+err.Error()
		return r
	}
	var pr pageResp
	if err := json.Unmarshal(data, &pr); err != nil {
		r.Status, r.Why = "failed", err.Error()
		return r
	}
	r.Version = pr.BodyVersion // the version the body is: a republish since the listing is the new current
	light, dark, why := thumbnails(e, pr.Body)
	if light == nil && dark == nil {
		if why == "" {
			why = "thumbnails are turned off (COMEBYE_NO_THUMBNAILS)"
		}
		r.Status, r.Why = "failed", why
		return r
	}
	if err := putThumbs(c, p.ID, pr.BodyVersion, light, dark); err != nil {
		r.Status, r.Why = "failed", "the hub did not take it: "+err.Error()
		return r
	}
	r.Status = "stored"
	if why != "" {
		r.Why = "only one scheme: " + why
	}
	return r
}

// backfillThumbs runs the backfill and prints a line per page and a summary.
// It returns an error (exit status 1) when any page failed.
func backfillThumbs(e *Env, c *client, ids []string, dry bool) error {
	pages, err := backfillTargets(c, ids)
	if err != nil {
		return err
	}
	results := make([]backfillResult, len(pages))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, backfillJobs)
	for i, p := range pages {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r := backfillOne(e, c, p, dry)
			mu.Lock()
			results[i] = r
			if r.Status != "skipped" || dry {
				fmt.Fprintln(e.Stdout, backfillLine(r))
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	counts := map[string]int{}
	reasons := map[string]int{}
	var took time.Duration
	var rendered int
	for _, r := range results {
		counts[r.Status]++
		if r.Status == "skipped" {
			reasons[r.Why]++
		}
		if r.Took > 0 {
			took += r.Took
			rendered++
		}
	}
	var keys []string
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sk []string
	for _, k := range keys {
		sk = append(sk, fmt.Sprintf("%d %s", reasons[k], k))
	}
	fmt.Fprintf(e.Stdout, "\n%d stored, %d skipped, %d failed (of %d pages).", counts["stored"], counts["skipped"], counts["failed"], len(pages))
	if len(sk) > 0 {
		fmt.Fprintf(e.Stdout, " Skipped: %s.", strings.Join(sk, "; "))
	}
	if rendered > 0 {
		fmt.Fprintf(e.Stdout, " %s a page on average.", (took / time.Duration(rendered)).Round(10*time.Millisecond))
	}
	fmt.Fprintln(e.Stdout)
	if counts["failed"] > 0 {
		return fmt.Errorf("%d page(s) failed", counts["failed"])
	}
	return nil
}

func backfillLine(r backfillResult) string {
	s := fmt.Sprintf("%-8s %s v%d", r.Status, r.ID, r.Version)
	if r.Took.Round(10*time.Millisecond) > 0 {
		s += fmt.Sprintf(" (%s)", r.Took.Round(10*time.Millisecond))
	}
	if r.Why != "" {
		s += ": " + r.Why
	}
	return s
}
