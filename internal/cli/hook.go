package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jhjaggars/comebye/internal/question"
)

// `comebye hook stop` is the body of a Claude Code Stop hook (the
// comebye-hub mod calls it from its classic.Stop hook; a settings hook can
// run it directly). The hook's JSON arrives on stdin. When the turn ended
// with questions or decisions for the user and the agent did not run
// `comebye todo add` in that turn, it answers {"decision":"block",...}, so
// the agent files them before stopping. It speaks once per turn: a Stop
// that follows a block arrives with stop_hook_active set and gets silence.
// Whatever goes wrong, it says nothing and exits 0: a hook that fails must
// not keep a session from stopping.

type stopInput struct {
	StopHookActive    bool   `json:"stop_hook_active"`
	LastAssistantText string `json:"last_assistant_message"`
	TranscriptPath    string `json:"transcript_path"`
}

func cmdHook(e *Env, args []string) error {
	if len(args) == 0 || args[0] != "stop" {
		return usageErr("want hook stop")
	}
	var in stopInput
	data, err := io.ReadAll(io.LimitReader(e.Stdin, 4<<20))
	if err != nil || json.Unmarshal(data, &in) != nil {
		return nil
	}
	if reason := stopReminder(in); reason != "" {
		return json.NewEncoder(e.Stdout).Encode(map[string]any{"decision": "block", "reason": reason})
	}
	return nil
}

// stopReminder is the reason to hand the agent, "" for silence.
func stopReminder(in stopInput) string {
	if in.StopHookActive {
		return "" // already reminded this turn
	}
	ex := question.Extract(in.LastAssistantText)
	if len(ex.Items) == 0 {
		return ""
	}
	if filedInTurn(in.TranscriptPath) {
		return ""
	}
	var b strings.Builder
	n := len(ex.Items)
	fmt.Fprintf(&b, "Comebye: your last message leaves %d question%s for the user, and this turn did not run `comebye todo add`. A question left only in the chat pane is easily missed. ", n, map[bool]string{true: "", false: "s"}[n == 1])
	b.WriteString("File each one now, one item per question, in your own words: `comebye todo add \"<question>\" --kind decision --waiting \"$me\" --option \"label|detail\" ... --recommend N --context -` (the choices as --option, your pick as --recommend; $me is your agent id from `comebye whoami --json`). Then stop. This reminder comes once; do not repeat the questions in chat.\n\nWhat I found:\n")
	for i, it := range ex.Items {
		fmt.Fprintf(&b, "%d. %s", i+1, it.Question)
		if len(it.Options) > 0 {
			var labels []string
			for _, o := range it.Options {
				labels = append(labels, o.Label)
			}
			fmt.Fprintf(&b, " [options: %s]", strings.Join(labels, " | "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// filedInTurn reports that the transcript's current turn (after the last
// real user message) ran a Bash call with `comebye todo add`. A transcript
// it cannot read says no, so the reminder errs towards being given.
func filedInTurn(path string) bool {
	if path == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	const tail = 4 << 20
	if st, err := f.Stat(); err == nil && st.Size() > tail {
		_, _ = f.Seek(st.Size()-tail, io.SeekStart)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	found := false
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"type"`)) {
			continue
		}
		var row struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		switch row.Type {
		case "user":
			if realUserMessage(row.Message.Content) {
				found = false // a new turn begins
			}
		case "assistant":
			var blocks []struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Input struct {
					Command string `json:"command"`
				} `json:"input"`
			}
			if json.Unmarshal(row.Message.Content, &blocks) != nil {
				continue
			}
			for _, bl := range blocks {
				if bl.Type == "tool_use" && bl.Name == "Bash" && strings.Contains(bl.Input.Command, "comebye todo add") {
					found = true
				}
			}
		}
	}
	return found
}

// realUserMessage: a person's prompt, not the tool results Claude files as
// user rows.
func realUserMessage(raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s) != ""
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "text" {
			return true
		}
	}
	return false
}
