package usage

import "strings"

// ContextWindow is the context window, in tokens, of a model the transcripts
// name (Claude's model ids), and 0 when it is not known: a window is never
// guessed. tokens is the context in use now. Claude models with a 1M-token
// option do not say in the transcript which was chosen, so a context past the
// smaller window proves the larger one.
//
// Sizes: the Claude 4.x line is 200k (Opus 4.6+ and Sonnet 4.6 can run at
// 1M). The 5.x Opus, Sonnet and Fable models are 1M: sessions here carry
// more than 970k tokens in one request on them. Haiku 5.x has no size on
// record, so it is unknown. Pi's and OpenCode's models are read from their
// own config files by the server (see internal/server/ctxwindow.go).
func ContextWindow(model string, tokens int) int {
	m := strings.ToLower(model)
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	m = strings.TrimSuffix(m, "[1m]")
	switch {
	case strings.HasPrefix(m, "claude-opus-5"), strings.HasPrefix(m, "claude-sonnet-5"), strings.HasPrefix(m, "claude-fable-5"):
		return 1_000_000
	case strings.HasPrefix(m, "claude-opus-4"), strings.HasPrefix(m, "claude-sonnet-4"), strings.HasPrefix(m, "claude-haiku-4"), strings.HasPrefix(m, "claude-3"):
		if tokens > 200_000 {
			return 1_000_000
		}
		return 200_000
	}
	return 0
}
