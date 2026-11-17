package transcript

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// maxEditChars caps the text one tool message carries for a diff: the sum of
// every old and new string. Past it the strings are cut and Truncated is set,
// so a rewritten big file does not bloat the response.
const maxEditChars = 12000

// EditInfo is what an Edit, MultiEdit or Write call changed, for the app's
// diff view. It is the call's input in a fixed shape, not a patch: the app
// draws the diff.
type EditInfo struct {
	Path string `json:"path,omitempty"`
	// Write marks a file written whole: Edits holds one pair with no Old,
	// and New is the file's content.
	Write bool       `json:"write,omitempty"`
	Edits []EditPair `json:"edits"`
	// Truncated says some text was cut to fit maxEditChars.
	Truncated bool `json:"truncated,omitempty"`
}

// EditPair is one replacement: Old was found in the file and became New.
type EditPair struct {
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
	// All is Claude's replace_all.
	All bool `json:"all,omitempty"`
}

// parseEdit returns the diff info for an edit-like call, or nil when the tool
// is not one or its input has no usable text. Claude: Edit (file_path,
// old_string, new_string), MultiEdit (file_path, edits[]), Write (file_path,
// content). Pi: edit (path, oldText, newText, or edits[] of those) and
// write (path, content).
func parseEdit(tool string, raw json.RawMessage) *EditInfo {
	if len(raw) == 0 {
		return nil
	}
	var in struct {
		FilePath   string `json:"file_path"`
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		OldText    string `json:"oldText"`
		NewText    string `json:"newText"`
		ReplaceAll bool   `json:"replace_all"`
		Content    string `json:"content"`
		Edits      []struct {
			OldString  string `json:"old_string"`
			NewString  string `json:"new_string"`
			OldText    string `json:"oldText"`
			NewText    string `json:"newText"`
			ReplaceAll bool   `json:"replace_all"`
		} `json:"edits"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	e := &EditInfo{Path: in.FilePath}
	if e.Path == "" {
		e.Path = in.Path
	}
	first := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	switch strings.ToLower(tool) {
	case "edit", "multiedit", "str_replace", "str_replace_edit":
		for _, x := range in.Edits {
			e.Edits = append(e.Edits, EditPair{Old: first(x.OldString, x.OldText), New: first(x.NewString, x.NewText), All: x.ReplaceAll})
		}
		if o, n := first(in.OldString, in.OldText), first(in.NewString, in.NewText); o != "" || n != "" {
			e.Edits = append([]EditPair{{Old: o, New: n, All: in.ReplaceAll}}, e.Edits...)
		}
	case "write":
		if in.Content == "" {
			return nil
		}
		e.Write = true
		e.Edits = []EditPair{{New: in.Content}}
	default:
		return nil
	}
	if len(e.Edits) == 0 {
		return nil
	}
	budget := maxEditChars
	cut := func(s string) string {
		if len(s) <= budget {
			budget -= len(s)
			return s
		}
		e.Truncated = true
		i := budget
		for i > 0 && !utf8.RuneStart(s[i]) { // cut on a rune boundary
			i--
		}
		s = s[:i]
		budget = 0
		return s
	}
	for i := range e.Edits {
		e.Edits[i].Old = cut(e.Edits[i].Old)
		e.Edits[i].New = cut(e.Edits[i].New)
	}
	return e
}
