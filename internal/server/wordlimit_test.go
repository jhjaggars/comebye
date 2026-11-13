package server

import (
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/playbook"
)

// maxPlaybookWords is the ceiling for the coordinator playbook plus the
// built-in handoff template, counted as whitespace-separated words. Every
// coordinator and every worker reads these in full, so growth has to prune
// or move content out (into docs/) rather than add. Raise it only on
// purpose, in the same change that explains why.
const maxPlaybookWords = 7350

func TestPlaybookWordLimit(t *testing.T) {
	pb := len(strings.Fields(playbook.Body()))
	tpl := len(strings.Fields(defaultHandoffTemplate))
	t.Logf("playbook %d + handoff template %d = %d words (limit %d)", pb, tpl, pb+tpl, maxPlaybookWords)
	if pb+tpl > maxPlaybookWords {
		t.Errorf("playbook (%d words) + handoff template (%d words) = %d, over the limit of %d: prune or move content to docs/ instead of raising it (see CONTRIBUTING.md)", pb, tpl, pb+tpl, maxPlaybookWords)
	}
}
