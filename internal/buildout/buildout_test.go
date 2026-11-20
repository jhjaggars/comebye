package buildout

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func has(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestClear(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "precious.txt"), "keep me")

	run(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "Cargo.toml"), "[package]\n")
	write(t, filepath.Join(repo, ".gitignore"), "target/\ncache/\nnode_modules_ign/\n.venv/\nlink/\n")
	// Tracked build/ is kept even though the name is on the list.
	write(t, filepath.Join(repo, "build", "keep.go"), "package x")
	// A tracked file under an ignored name stays too.
	write(t, filepath.Join(repo, "src", "main.rs"), "fn main(){}")
	run(t, repo, "add", "Cargo.toml", ".gitignore", "build/keep.go", "src/main.rs")
	run(t, repo, "commit", "-q", "-m", "init")

	// Ignored target with a CACHEDIR.TAG and a Cargo.toml beside: cleared.
	write(t, filepath.Join(repo, "target", "CACHEDIR.TAG"), cacheTagSig+"\n")
	write(t, filepath.Join(repo, "target", "debug", "big.bin"), "0123456789")
	// Ignored directory carrying the tag but not on the list: cleared.
	write(t, filepath.Join(repo, "cache", "CACHEDIR.TAG"), cacheTagSig+"\n")
	write(t, filepath.Join(repo, "cache", "x"), "x")
	// Un-ignored node_modules: kept.
	write(t, filepath.Join(repo, "node_modules", "pkg", "i.js"), "x")
	// Ignored .venv: cleared.
	write(t, filepath.Join(repo, ".venv", "bin", "python"), "x")
	// Ignored dir with a tag that lies (no signature) and not on the list: kept.
	write(t, filepath.Join(repo, "node_modules_ign", "CACHEDIR.TAG"), "nope")
	// Untracked, un-ignored source: kept.
	write(t, filepath.Join(repo, "notes.md"), "wip")
	// A symlink to outside, ignored by name: not followed, target survives.
	if err := os.Symlink(outside, filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	// A symlink named target pointing outside is not a directory to clear.
	// (covered by `link`; also check a nested symlink inside cleared dir)
	if err := os.Symlink(outside, filepath.Join(repo, "target", "out")); err != nil {
		t.Fatal(err)
	}

	freed, cleared, err := Clear(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if freed < 10 || len(cleared) != 3 {
		t.Fatalf("freed %d, cleared %v", freed, cleared)
	}
	for _, gone := range []string{"target", "cache", ".venv"} {
		if has(filepath.Join(repo, gone)) {
			t.Errorf("%s should be cleared", gone)
		}
	}
	for _, kept := range []string{"build/keep.go", "node_modules/pkg/i.js", "node_modules_ign/CACHEDIR.TAG", "notes.md", "src/main.rs", "link"} {
		if !has(filepath.Join(repo, kept)) {
			t.Errorf("%s should be kept", kept)
		}
	}
	if !has(filepath.Join(outside, "precious.txt")) {
		t.Fatal("followed a symlink out of the worktree")
	}
}

func TestIgnoredNodeModulesAndNestedTarget(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "init", "-q")
	write(t, filepath.Join(repo, ".gitignore"), "node_modules/\ntarget/\ndist/\n")
	write(t, filepath.Join(repo, "crates", "a", "Cargo.toml"), "x")
	write(t, filepath.Join(repo, "crates", "a", "target", "f"), "1234")
	write(t, filepath.Join(repo, "web", "node_modules", "m", "f"), "12")
	write(t, filepath.Join(repo, "web", "dist", "f"), "1")
	// target without Cargo.toml beside: kept.
	write(t, filepath.Join(repo, "other", "target", "f"), "1")
	run(t, repo, "add", ".gitignore", "crates/a/Cargo.toml")
	run(t, repo, "commit", "-q", "-m", "x")

	dirs, err := Find(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range dirs {
		got[d.Rel] = true
	}
	for _, want := range []string{"crates/a/target", "web/node_modules", "web/dist"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if got["other/target"] || len(got) != 3 {
		t.Errorf("unexpected: %v", got)
	}
}

func TestNotAWorktree(t *testing.T) {
	if _, err := Find(context.Background(), t.TempDir()); err == nil {
		t.Fatal("want an error outside a git checkout")
	}
}
