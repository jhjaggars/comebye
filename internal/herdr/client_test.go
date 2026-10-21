package herdr

import "testing"

func TestValidKey(t *testing.T) {
	for _, k := range []string{"esc", "enter", "tab", "space", "up", "down", "left", "right", "ctrl+c", "1", "4", "9", "y", "n"} {
		if !ValidKey(k) {
			t.Errorf("ValidKey(%q) = false, want true", k)
		}
	}
	// Anything that could type free text or escape the dialog keys stays out.
	for _, k := range []string{"", "0", "10", "a", "Y", "N", "yes", "ctrl+x", "alt+y", "shift+tab", "rm -rf /", "\n", "\x1b"} {
		if ValidKey(k) {
			t.Errorf("ValidKey(%q) = true, want false", k)
		}
	}
}
