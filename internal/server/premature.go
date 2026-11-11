package server

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// A worker that goes idle while a backgrounded test or monitor is still
// running looks, to the idle-after-working guess, like a worker that
// finished; its "report" is then a status line ("the uitest run is still
// going"). The coordinator was told "finished" about 8 times for workers
// that were not. The hub holds that notice instead:
//
//   - while the worker's session shows a background task still open (the
//     task's end wakes the session, which sets the entry running again and
//     drops the notice; a later idle makes a fresh one); or
//   - while the captured text looks like a progress line rather than a
//     report, for at most progressHold, since "when unsure, notify".
//
// A report the worker filed itself (`comebye report`) is never held.

const (
	// progressHold bounds how long a progress-looking guess holds a notice
	// when the session cannot say what it has running.
	progressHold = 15 * time.Minute
	// progressMaxLines and progressMaxBytes bound a "progress line".
	progressMaxLines = 4
	progressMaxBytes = 500
)

var (
	reportHeadingRE = regexp.MustCompile(`(?mi)^\s{0,3}#{1,6}\s*(summary|verification|changes|follow-ups?|decisions for the user)\b`)
	progressWordsRE = regexp.MustCompile(`(?i)\b(still|in progress|running|waiting|monitor(ing)?|background|in the background|not (yet )?(done|finished|complete)|will (report|finish|continue|check|follow)|once (it|the|that|they)|until|meanwhile|checking)\b`)
)

// looksLikeProgress says whether a guessed report reads as a status line:
// short, no report headings, and worded as something still going on.
func looksLikeProgress(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || len(t) > progressMaxBytes || reportHeadingRE.MatchString(t) {
		return false
	}
	if strings.Count(t, "\n")+1 > progressMaxLines {
		return false
	}
	return progressWordsRE.MatchString(t)
}

// holdFinished says why the "worker finished" notice for e should wait now
// ("" to send it).
func (s *Server) holdFinished(ctx context.Context, e ledger.Entry, now time.Time) string {
	if e.Status != ledger.Finished || e.ReportSource == "explicit" || e.LandCheck != nil {
		return ""
	}
	if tasks, known := s.backgroundOf(ctx, e, true); known && len(tasks) > 0 {
		return "background task still running" + parenthesized(tasks[0].Desc)
	}
	if looksLikeProgress(e.FinalReport) && e.FinishedAt != nil && now.Sub(*e.FinishedAt) < progressHold {
		return "the captured text reads as a progress line"
	}
	return ""
}
