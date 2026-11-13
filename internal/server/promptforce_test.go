package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestPromptUpdateNoticeScreenNotRefused(t *testing.T) {
	for _, f := range []string{"claude_update_notice.ansi", "claude_update_notice.txt"} {
		s, fh, _ := coordServer(t)
		showScreen(fh, "w1:p1", capture(t, f))
		w := do(s, "POST", "/api/agents/coordinator/prompt", userHdr, `{"text":"hello"}`)
		if w.Code != http.StatusOK || fh.call("agent.prompt") == nil {
			t.Errorf("%s: prompt = %d %s", f, w.Code, w.Body)
		}
	}
}

func TestPromptSendAnywayPastTypingHold(t *testing.T) {
	s, fh, _ := coordServer(t)
	showScreen(fh, "w1:p1", capture(t, "claude_typed.ansi"))
	w := do(s, "POST", "/api/agents/coordinator/prompt", userHdr, `{"text":"hello"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "input_busy") || !strings.Contains(w.Body.String(), "I think we should") {
		t.Fatalf("held prompt = %d %s, want 409 input_busy naming the text", w.Code, w.Body)
	}
	w = do(s, "POST", "/api/agents/coordinator/prompt", userHdr, `{"text":"hello","force":true}`)
	if w.Code != http.StatusOK || fh.call("agent.prompt") == nil {
		t.Fatalf("forced prompt = %d %s", w.Code, w.Body)
	}
	// A dialog on screen still refuses, forced or not.
	showScreen(fh, "w1:p1", claudePermScreen)
	w = do(s, "POST", "/api/agents/coordinator/prompt", userHdr, `{"text":"hello","force":true}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "dialog_open") {
		t.Fatalf("forced prompt at a dialog = %d %s, want 409 dialog_open", w.Code, w.Body)
	}
}
