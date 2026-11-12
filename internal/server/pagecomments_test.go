package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// quietQueue gives the prompt queue a fake send and, at the end of the test,
// waits for deliveries still running so none writes into the removed state dir.
func quietQueue(t *testing.T, s *Server) {
	t.Helper()
	s.pq.send = func(context.Context, herdr.AgentInfo, string) error { return nil }
	t.Cleanup(func() {
		for i := 0; i < 200; i++ {
			s.pq.mu.Lock()
			n := len(s.pq.sending)
			s.pq.mu.Unlock()
			if n == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func commentsPage(t *testing.T, s *Server) Page {
	t.Helper()
	w := do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"body": "<title>Plan</title><body><h1>hi</h1></body>"}))
	if w.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", w.Code, w.Body)
	}
	return pageOf(t, w.Body.Bytes())
}

func TestPageComments(t *testing.T) {
	s, _, _ := todoServer(t)
	quietQueue(t, s)
	var sentTo []string
	p := commentsPage(t, s)
	url := "/api/pages/" + p.ID + "/comments"

	w := do(s, "POST", url, userHdr, `{"version":1,"comments":[{"quote":"cut over on Friday","before":"We will ","after":" without","note":"Do a dry run first."}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("comments = %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"sent":"queued"`) {
		t.Fatalf("not queued: %s", w.Body)
	}
	items := s.pq.list("", true)
	for _, it := range items {
		sentTo = append(sentTo, it.Agent+": "+it.Text)
	}
	if len(sentTo) != 1 || !strings.HasPrefix(sentTo[0], "desk/w2:p1: The user commented on your page") || !strings.Contains(sentTo[0], "cut over on Friday") {
		t.Fatalf("queued = %q", sentTo)
	}
	todos := s.todos.list("all")
	if len(todos) != 1 || todos[0].Kind != "followup" || !strings.Contains(todos[0].Context, "> cut over on Friday") || !strings.Contains(todos[0].Question, "Comments on Plan (v1)") {
		t.Fatalf("todos = %+v", todos)
	}
}

func TestPageCommentsRefusals(t *testing.T) {
	s, _, _ := todoServer(t)
	quietQueue(t, s)
	p := commentsPage(t, s)
	url := "/api/pages/" + p.ID + "/comments"
	ok := `{"quote":"q","note":"n"}`
	cases := []struct {
		name, hdr, body string
		want            int
	}{
		{"an agent token", "agent", `{"version":1,"comments":[` + ok + `]}`, http.StatusForbidden},
		{"a recipient in the body", "", `{"version":1,"agent":"desk/w1:p1","comments":[` + ok + `]}`, http.StatusBadRequest},
		{"a recipient in a comment", "", `{"version":1,"comments":[{"quote":"q","note":"n","agent":"x/y"}]}`, http.StatusBadRequest},
		{"no comments", "", `{"version":1,"comments":[]}`, http.StatusBadRequest},
		{"no note", "", `{"version":1,"comments":[{"quote":"q","note":" "}]}`, http.StatusBadRequest},
		{"a long quote", "", `{"version":1,"comments":[{"quote":"` + strings.Repeat("x", 501) + `","note":"n"}]}`, http.StatusBadRequest},
		{"a long note", "", `{"version":1,"comments":[{"quote":"q","note":"` + strings.Repeat("x", 1001) + `"}]}`, http.StatusBadRequest},
		{"too many", "", `{"version":1,"comments":[` + strings.Repeat(ok+",", 20) + ok + `]}`, http.StatusBadRequest},
		{"a version that is not there", "", `{"version":2,"comments":[` + ok + `]}`, http.StatusBadRequest},
		{"too big", "", `{"version":1,"comments":[{"quote":"q","note":"` + strings.Repeat("x", 70<<10) + `"}]}`, http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		hdr := userHdr
		if c.hdr == "agent" {
			hdr = agentHdr("w2:p1")
		}
		if w := do(s, "POST", url, hdr, c.body); w.Code != c.want {
			t.Errorf("%s = %d %s, want %d", c.name, w.Code, w.Body, c.want)
		}
	}
	if w := do(s, "POST", "/api/pages/pg_0000000000/comments", userHdr, `{"version":1,"comments":[`+ok+`]}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown page = %d", w.Code)
	}
	if n := len(s.todos.list("all")); n != 0 {
		t.Errorf("refused batches made %d to-do items", n)
	}
}

func TestPageCommentsRecipientIsThePublisher(t *testing.T) {
	s, _, _ := todoServer(t)
	quietQueue(t, s)
	p := commentsPage(t, s) // published by desk/w2:p1
	do(s, "POST", "/api/pages/"+p.ID+"/comments", userHdr, `{"version":1,"comments":[{"quote":"hi","note":"n"}]}`)
	items := s.pq.list("", true)
	if len(items) != 1 || items[0].Agent != "desk/w2:p1" {
		t.Fatalf("queued = %+v", items)
	}
	// A page the user published has no agent to message: the to-do item is the record.
	w := do(s, "POST", "/api/pages", userHdr, pubJSON(map[string]any{"body": "<h1>mine</h1>"}))
	mine := pageOf(t, w.Body.Bytes())
	w = do(s, "POST", "/api/pages/"+mine.ID+"/comments", userHdr, `{"version":1,"comments":[{"quote":"mine","note":"n"}]}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"sent":"none"`) {
		t.Fatalf("own page = %d %s", w.Code, w.Body)
	}
}

func TestPageCommentsRate(t *testing.T) {
	s, _, _ := todoServer(t)
	quietQueue(t, s)
	p := commentsPage(t, s)
	got := 0
	for i := 0; i < commentRate+2; i++ {
		body := `{"version":1,"comments":[{"quote":"q` + strings.Repeat("a", i) + `","note":"n"}]}`
		if w := do(s, "POST", "/api/pages/"+p.ID+"/comments", userHdr, body); w.Code == http.StatusTooManyRequests {
			got++
		}
	}
	if got == 0 {
		t.Fatal("no batch was rate limited")
	}
}

func TestPageServedWithReporter(t *testing.T) {
	s, _, _ := todoServer(t)
	quietQueue(t, s)
	p := commentsPage(t, s)
	w := do(s, "GET", "/p/"+p.ID, userHdr, "")
	b := w.Body.String()
	if w.Code != 200 || strings.Count(b, "comebye-selection") != 1 || strings.Index(b, "comebye-selection") > strings.LastIndex(b, "</body>") {
		t.Fatalf("served = %d %s", w.Code, b)
	}
	// The stored source is untouched.
	w = do(s, "GET", "/api/pages/"+p.ID+"?body=1", userHdr, "")
	if strings.Contains(w.Body.String(), "comebye-selection") {
		t.Fatal("the reporter was stored with the page")
	}
	// No </body>: appended. Markdown shell: present.
	if got := string(withReporter([]byte("<h1>x</h1>"))); !strings.HasPrefix(got, "<h1>x</h1><script>") {
		t.Fatalf("no body tag: %s", got)
	}
	md := do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"body": "# notes\ntext"}))
	mp := pageOf(t, md.Body.Bytes())
	if g := do(s, "GET", "/p/"+mp.ID, userHdr, "").Body.String(); !strings.Contains(g, "comebye-selection") {
		t.Fatal("markdown page has no reporter")
	}
}

func TestCommentsPromptFits(t *testing.T) {
	var cs []pageComment
	for i := 0; i < commentMaxBatch; i++ {
		cs = append(cs, pageComment{Quote: strings.Repeat("q", 500), Note: strings.Repeat("n", 1000)})
	}
	got := commentsPrompt(Page{ID: "pg_1", Title: "T"}, 1, "http://x/p/pg_1/1", "t1", cs)
	if len(got) > queueMaxText || !strings.Contains(got, "more in the to-do item") || !strings.Contains(got, "t1") {
		t.Fatalf("prompt %d bytes: %.200s…", len(got), got)
	}
	if md := commentsMarkdown(cs, todoMaxContext-512); len(md) > todoMaxContext {
		t.Fatalf("markdown %d bytes", len(md))
	}
}
