package apipy

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The module's own unit tests (tests/test_records.py), with python3.
func TestPythonUnit(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH")
	}
	cmd := exec.Command(py, "-B", "-m", "unittest", "discover", "-s", "tests", "-p", "test_*.py")
	cmd.Env = append(os.Environ(), "PYTHONPATH=.", "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python unit tests: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("python unit tests: %s", out)
	}
}

func TestEmbedded(t *testing.T) {
	for _, f := range []string{"comebye/__init__.py", "comebye/_record.py", "comebye/_types.py", "comebye/gh.py"} {
		if _, err := Module().Open(f); err != nil {
			t.Errorf("%s is not embedded: %v", f, err)
		}
	}
	if !strings.Contains(Help, "comebye api run") || !strings.Contains(Types, "Entry  (server.entryView)") {
		t.Error("the guide or the reference is missing")
	}
}
