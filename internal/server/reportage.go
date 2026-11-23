package server

import (
	"fmt"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// reportAge says how old an entry's report is, for a reader who must tell
// an old report from a new one: "report 3 from 2 h ago". Empty when there
// is no report.
func reportAge(e ledger.Entry, now time.Time) string {
	if e.FinalReport == "" {
		return ""
	}
	what := "report"
	if e.ReportSource == "explicit" && e.ReportSeq > 1 {
		what = fmt.Sprintf("report %d", e.ReportSeq)
	} else if e.ReportSource != "explicit" {
		what = "report (its last reply, taken by the hub)"
	}
	if e.ReportAt == nil {
		return what + " from an unrecorded time (before report times were kept)"
	}
	return what + " from " + agoText(now, *e.ReportAt)
}

// newerReplyAge says how old the reply kept beside an older report is
// ("" when there is none newer).
func newerReplyAge(e ledger.Entry, now time.Time) string {
	if !e.NewerReply() {
		return ""
	}
	return agoText(now, *e.LastPassReportAt)
}

// reportAgeMarker starts the sentence a notice ends with on the report's
// age. A notice's key leaves it out: the same report told twice is the same
// notice, however old it has grown.
const reportAgeMarker = " Report age: "

// reportAgeNote is the notice's sentence on the age of the report it
// points at. Only fixed words, times, numbers and the entry's safe id
// appear in it. The times are measured from when the notice was queued
// (now, when none is): the text of one notice stays the same across
// retries, which find a late earlier attempt by a hash of that text.
func reportAgeNote(e ledger.Entry, now time.Time) string {
	if e.FinalReport == "" {
		return ""
	}
	ref := now
	if e.Callback != nil && !e.Callback.At.IsZero() {
		ref = e.Callback.At
	}
	what := "report"
	if e.ReportSource == "explicit" && e.ReportSeq > 1 {
		what = fmt.Sprintf("report %d", e.ReportSeq)
	} else if e.ReportSource != "explicit" {
		what = "report (its last reply, taken by the hub)"
	}
	t := reportAgeMarker + "the one on file is " + what
	if e.ReportAt == nil {
		t += ", from an unrecorded time (before report times were kept)"
	} else {
		t += ", written " + noticeTime(*e.ReportAt) + ", " + beforeNotice(ref, *e.ReportAt)
	}
	if e.NewerReply() {
		t += fmt.Sprintf("; the worker's last reply, written %s, is newer and is not a report (`comebye read %s --report` shows both)", noticeTime(*e.LastPassReportAt), safeText(e.ID))
	}
	return t + "."
}

// noticeTime is a time as a notice gives it: to the minute, in UTC.
func noticeTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

// beforeNotice says how long before the notice (ref) t was.
func beforeNotice(ref, t time.Time) string {
	d := ref.Sub(t)
	switch {
	case d < time.Minute:
		return "just before this notice"
	case d < time.Hour:
		return fmt.Sprintf("%d min before this notice", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h before this notice", int(d.Hours()))
	}
	return fmt.Sprintf("%d d before this notice", int(d.Hours()/24))
}

// withoutReportAge drops the report-age sentence from a notice's text.
func withoutReportAge(text string) string {
	before, _, _ := strings.Cut(text, reportAgeMarker)
	return before
}
