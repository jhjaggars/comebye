package server

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The "GPG is locked" item outlives its request: the host renews the request
// on the same item, the old one stays answerable until its own time, and one
// push goes out per item.
func TestGPGUnlockItemIsRenewedInPlace(t *testing.T) {
	s, h, a := passkeyServer(t)
	pushes := 0
	s.approvalPush = func(id, title, body string) { pushes++ }
	n := 0
	file := func(extra string) (int, string, string) {
		n++ // the host's own request: a fresh nonce each time
		code, out := fileApproval(t, s, agentHdr("w2:p1"), fmt.Sprintf(`{"action":"gpg-unlock","subject":"k","request":{"v":1,"n":%d}%s}`, n, extra))
		todo := ""
		if out.Todo != nil {
			todo = out.Todo.ID
		}
		return code, out.Approval.ID, todo
	}
	_, ap1, todo := file("")
	if todo == "" || pushes != 1 {
		t.Fatalf("first: todo %q pushes %d", todo, pushes)
	}
	// Many renewals, each before the last runs out: one item, one push.
	now := time.Now()
	ids := []string{ap1}
	for i := 0; i < 20; i++ {
		now = now.Add(75 * time.Second)
		s.approvals.mu.Lock()
		s.approvals.now = func() time.Time { return now }
		s.approvals.mu.Unlock()
		code, ap, td := file("")
		if code != 201 || ap == "" {
			t.Fatalf("renewal %d = %d", i, code)
		}
		if td != todo {
			t.Fatalf("renewal %d made another item %q (was %q)", i, td, todo)
		}
		ids = append(ids, ap)
	}
	if pushes != 1 {
		t.Fatalf("%d pushes for one item", pushes)
	}
	if n := len(s.todos.list("")); n != 1 {
		t.Fatalf("%d active items", n)
	}
	it, _ := s.todos.get(todo)
	if it.State != TodoOpen || it.Approval.ID != ids[len(ids)-1] || it.Approval.State != apPending {
		t.Fatalf("item = %+v / %+v", it, it.Approval)
	}
	// The previous request is still answerable (a ceremony begun on it lands);
	// the ones before that are spent.
	if code, body := assertFor(t, s, h, ids[len(ids)-3], a); code != 409 {
		t.Errorf("two renewals back = %d %s", code, body)
	}
	if code, body := assertFor(t, s, h, ids[len(ids)-2], a); code != 200 {
		t.Fatalf("the previous request = %d %s", code, body)
	}
	if it, _ := s.todos.get(todo); it.State != TodoDone {
		t.Fatalf("after the answer: %+v", it)
	}
}

// A request that runs out leaves the item open; the host's next request takes
// it over, and tapping gets a fresh challenge from the same item.
func TestGPGUnlockItemSurvivesItsRequestExpiring(t *testing.T) {
	s, h, a := passkeyServer(t)
	pushes := 0
	s.approvalPush = func(id, title, body string) { pushes++ }
	_, out := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"k"}`)
	todo := out.Todo.ID
	now := time.Now().Add(approvalTTL + 10*time.Second)
	s.approvals.mu.Lock()
	s.approvals.now = func() time.Time { return now }
	s.approvals.mu.Unlock()
	if code, body := assertFor(t, s, h, out.Approval.ID, a); code != 409 || !strings.Contains(body, "expired") {
		t.Fatalf("expired = %d %s", code, body)
	}
	it, _ := s.todos.get(todo)
	if it.State != TodoOpen || it.Approval.State != apExpired {
		t.Fatalf("expired item = %+v / %+v", it, it.Approval)
	}
	code, out2 := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"k","request":{"v":1,"n":2}}`)
	if code != 201 || out2.Todo == nil || out2.Todo.ID != todo || out2.Approval.ID == out.Approval.ID {
		t.Fatalf("refile = %d %+v", code, out2)
	}
	if pushes != 1 {
		t.Fatalf("%d pushes", pushes)
	}
	if code, body := assertFor(t, s, h, out2.Approval.ID, a); code != 200 {
		t.Fatalf("answer after the 2 minute mark = %d %s", code, body)
	}
	if it, _ := s.todos.get(todo); it.State != TodoDone {
		t.Fatalf("item = %+v", it)
	}
}

// Withdraw takes the item and every request still answerable.
func TestGPGUnlockWithdrawTakesTheRenewalsToo(t *testing.T) {
	s, h, a := passkeyServer(t)
	_, o1 := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"k","request":{"v":1,"n":1}}`)
	_, o2 := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"k","request":{"v":1,"n":2}}`)
	if w := do(s, "POST", "/api/approvals/"+o2.Approval.ID+"/withdraw", agentHdr("w2:p1"), `{}`); w.Code != 200 {
		t.Fatalf("withdraw = %d %s", w.Code, w.Body)
	}
	if it, _ := s.todos.get(o1.Todo.ID); it.State != TodoDropped {
		t.Fatalf("item = %+v", it)
	}
	if code, body := assertFor(t, s, h, o1.Approval.ID, a); code != 409 {
		t.Fatalf("the older request after the withdrawal = %d %s", code, body)
	}
}

// "Not now" closes the item and the watcher's refiling is quiet for a while;
// a person's own ask (a worker's `comebye gpg unlock`) is not.
func TestGPGUnlockNotNowSnoozesTheWatcher(t *testing.T) {
	s, _, _ := passkeyServer(t)
	_, o := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"k","auto":true}`)
	if w := do(s, "POST", "/api/todos/"+o.Todo.ID+"/drop", userHdr, ``); w.Code != 200 {
		t.Fatalf("drop = %d %s", w.Code, w.Body)
	}
	w := do(s, "POST", "/api/approvals", agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"k","auto":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"snoozed":true`) {
		t.Fatalf("watcher after Not now = %d %s", w.Code, w.Body)
	}
	if n := len(s.todos.list("")); n != 0 {
		t.Fatalf("%d items after a snoozed ask", n)
	}
	if code, out := fileApproval(t, s, agentHdr("w2:p1"), `{"action":"gpg-unlock","subject":"k"}`); code != 201 || out.Todo == nil {
		t.Fatalf("a worker's ask = %d", code)
	}
}
