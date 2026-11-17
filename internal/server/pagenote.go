package server

import (
	"encoding/json"
	"net/http"
)

// A Page's chat helper hands findings back with one narrow route:
// POST /api/pages/{id}/note. It can file a follow-up to-do that links that
// Page, waiting on nobody, and nothing else: the kind, the link and the
// waiter are fixed here, not taken from the body. Only the Page's own chat
// helper (or the user) may call it, so the claude helper's allow-list names
// `comebye pages note`, not the general `comebye todo add`.

const pageNoteDetailMax = 4 << 10

func (s *Server) handlePageNote(w http.ResponseWriter, r *http.Request) {
	if s.todoOff(w, r) || s.pagesOff(w, r) {
		return
	}
	var b struct {
		Finding string `json:"finding"`
		Details string `json:"details"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	bad := func(code, msg string) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": code, "message": msg})
	}
	who := s.pageCaller(r)
	p, ok := s.pages.get(r.PathValue("id"))
	if !ok {
		pageErr(w, errPageNotFound)
		return
	}
	if !who.user && !s.chats.helperOf(p.ID, who.agent) {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "not_the_helper", "message": "only the chat helper of this Page can file a note about it"})
		return
	}
	q := oneLine(b.Finding, todoMaxQuestion)
	if q == "" {
		bad("bad_question", "a finding is required")
		return
	}
	if len(b.Details) > pageNoteDetailMax {
		bad("context_too_large", "the details are limited to 4096 bytes")
		return
	}
	page, err := s.pageFor(p.ID, 0)
	if err != nil {
		bad("bad_page", err.Error())
		return
	}
	c := callerOf(r)
	it := Todo{Question: q, Context: block(b.Details, pageNoteDetailMax), Kind: "followup", Page: page, CreatedBy: s.by(r, "")}
	if c.agent() {
		it.Source = SourceAgent
		if c.pane != "" {
			it.CreatedByID = s.replyID(c)
		}
	}
	out, dup, err := s.todos.add(it)
	if err != nil {
		todoErr(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]any{"todo": map[string]any{"id": out.ID, "kind": out.Kind}, "duplicate": dup})
}
