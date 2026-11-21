package server

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// docs/lifecycle.md states the re-ask age in words; it must say what reaskCap is.
func TestLifecycleDocNamesTheReaskCap(t *testing.T) {
	b, err := os.ReadFile("../../docs/lifecycle.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Join(strings.Fields(string(b)), " ")
	var age string
	switch {
	case reaskCap == time.Hour:
		age = "1 hour"
	case reaskCap%time.Hour == 0:
		age = fmt.Sprintf("%d hours", reaskCap/time.Hour)
	default:
		age = fmt.Sprintf("%d minutes", reaskCap/time.Minute)
	}
	if want := "unresolved, **older than " + age + "**"; !strings.Contains(doc, want) {
		t.Errorf("docs/lifecycle.md does not say the re-ask age is %s (reaskCap = %v): want %q", age, reaskCap, want)
	}
	if !strings.Contains(doc, "`reaskCap = time.") {
		t.Error("docs/lifecycle.md does not name the reaskCap constant")
	}
}
