package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/archive"
)

// A worker holds no archive key and has no route to the bucket. Its sessions
// are pulled over ssh to this machine, which uploads them under a host name of
// the worker's own. The worker is not trusted: what it sends is unpacked here
// by extractSessions, and it decides nothing about where it goes in the archive.

const (
	maxPullBytes = int64(10 << 30)
	maxPullFiles = 1_000_000
	// staleAfter: a pull's temporary copy older than this was left by a killed
	// run. It holds plaintext transcripts, so the next pull removes it.
	staleAfter = 24 * time.Hour
)

// pullScript runs on the worker and writes a gzipped tar of the agents' session
// directories to stdout. GNU tar exits 1 if a file changes as it reads, which is
// expected with agents running; the copy is a snapshot either way.
const pullScript = `cd "$HOME" || exit 1
set --
for d in .claude/projects .pi/agent/sessions; do [ -d "$d" ] && set -- "$@" "$d"; done
if [ $# -eq 0 ]; then exec tar -czf - --files-from=/dev/null; fi
tar -czf - --warning=no-file-changed --warning=no-file-removed --ignore-failed-read "$@"
rc=$?
[ "$rc" -le 1 ]`

// loadArchiveEnv reads the settings the archive timer uses, from a file of
// KEY=VALUE lines (systemd's EnvironmentFile). The values are locations and
// modes; the S3 secret stays in the credentials file it names.
func loadArchiveEnv(p string) (archive.Config, error) {
	var c archive.Config
	b, err := os.ReadFile(p)
	if err != nil {
		return c, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		k, v, ok := strings.Cut(l, "=")
		if !ok || strings.HasPrefix(l, "#") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		switch strings.TrimSpace(k) {
		case "COMEBYE_ARCHIVE_ENCRYPTION":
			c.Encryption = v
		case "COMEBYE_ARCHIVE_DIR":
			c.Dir = v
		case "COMEBYE_ARCHIVE_S3":
			c.S3 = v
		case "COMEBYE_ARCHIVE_S3_CREDENTIALS":
			c.S3Credentials = v
		case "COMEBYE_ARCHIVE_S3_REGION":
			c.S3Region = v
		case "COMEBYE_ARCHIVE_S3_PLAINTEXT":
			c.S3Plaintext = v
		case "COMEBYE_ARCHIVE_RECIPIENT":
			c.Recipient = v
		case "COMEBYE_ARCHIVE_IDENTITY":
			c.Identity = v
		}
	}
	return c, nil
}

// sessionRoots maps what the worker sends to where it is unpacked.
var sessionRoots = []struct{ remote, local string }{
	{".claude/projects/", "claude"},
	{".pi/agent/sessions/", "pi"},
}

// localName is where a tar entry goes under the unpack directory, or an error
// for a name a real worker would never send.
func localName(name string) (string, error) {
	if strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00") {
		return "", fmt.Errorf("refusing tar entry %q", name)
	}
	for _, e := range strings.Split(name, "/") {
		if e == ".." {
			return "", fmt.Errorf("refusing tar entry %q", name)
		}
	}
	clean := path.Clean(name)
	for _, r := range sessionRoots {
		if rest, ok := strings.CutPrefix(clean, r.remote); ok {
			return filepath.Join(r.local, filepath.FromSlash(rest)), nil
		}
	}
	return "", fmt.Errorf("refusing tar entry %q: not under a session directory", name)
}

// extractSessions unpacks a worker's tar under dir. It writes only regular files,
// private and under the two session roots, and creates no links, so nothing a
// worker sends can reach outside dir. It keeps modification times, which the
// archiver uses to tell whether a file's last line is still being written.
func extractSessions(r io.Reader, dir string, maxBytes int64, maxFiles int) (files int, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("reading the worker's sessions: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	left := maxBytes
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return files, fmt.Errorf("reading the worker's sessions: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeSymlink && h.Typeflag != tar.TypeLink {
				return files, fmt.Errorf("refusing tar entry %q: type %q", h.Name, h.Typeflag)
			}
			continue // a directory is made on demand; a link is never followed or made
		}
		rel, err := localName(h.Name)
		if err != nil {
			return files, err
		}
		if files++; files > maxFiles {
			return files, fmt.Errorf("the worker sent more than %d files", maxFiles)
		}
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return files, err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return files, err
		}
		n, err := io.Copy(f, io.LimitReader(tr, left+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return files, fmt.Errorf("reading the worker's sessions: %w", err)
		}
		if left -= n; left < 0 {
			return files, fmt.Errorf("the worker sent more than %d bytes of sessions", maxBytes)
		}
		if err := os.Chtimes(p, h.ModTime, h.ModTime); err != nil {
			return files, err
		}
	}
}

// pull streams a worker's sessions into dir.
func (e *env) pull(ctx context.Context, name, dir string) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	type result struct {
		files int
		err   error
	}
	done := make(chan result, 1)
	go func() {
		n, err := extractSessions(pr, dir, maxPullBytes, maxPullFiles)
		if err != nil {
			cancel() // stop ssh; there is no use in the rest
		}
		pr.CloseWithError(errors.New("extraction stopped"))
		done <- result{n, err}
	}()
	rerr := e.tr().Stream(ctx, name, pullScript, pw)
	pw.CloseWithError(rerr) // a nil error is a clean end of file
	res := <-done
	if res.err != nil {
		return res.files, res.err
	}
	return res.files, rerr
}

// pullDir makes a private directory for one pull, after removing what a killed
// run left behind.
func pullDir(c *Config, name string) (string, error) {
	base := filepath.Join(c.StateDir, "pull")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	if old, err := os.ReadDir(base); err == nil {
		for _, d := range old {
			if fi, err := d.Info(); err == nil && time.Since(fi.ModTime()) > staleAfter {
				os.RemoveAll(filepath.Join(base, d.Name()))
			}
		}
	}
	return os.MkdirTemp(base, name+"-")
}

// saveSessions pulls a running worker's sessions, uploads what the archive does
// not have yet and verifies the result. final says the agents have stopped, so
// every byte must now be in the archive.
func (e *env) saveSessions(ctx context.Context, inst *Inst, final bool) error {
	name := inst.Name
	if inst.ID == "" {
		return fmt.Errorf("the cloud gave no instance id for %s, so it has no archive host", name)
	}
	cfg, err := loadArchiveEnv(e.Cfg.ArchiveEnv)
	if err != nil {
		return fmt.Errorf("reading the archive settings (archive_env): %w", err)
	}
	cfg.Host = archive.WorkerHost(name, inst.ID)
	a, err := cfg.Open()
	if err != nil {
		return err
	}
	o, err := cfg.OpenIdentity()
	if err != nil {
		return fmt.Errorf("sessions could not be verified after upload: %w", err)
	}
	if err := a.ClaimWorker(archive.WorkerInfo{Name: name, Provider: e.Cfg.Provider, InstanceID: inst.ID, Created: inst.Created}); err != nil {
		return err
	}
	if err := e.connect(ctx, name); err != nil {
		return err
	}
	dir, err := pullDir(e.Cfg, name)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	files, err := e.pull(ctx, name, dir)
	if err != nil {
		return fmt.Errorf("pulling the sessions: %w", err)
	}
	cfg.ClaudeDir, cfg.PiDir = filepath.Join(dir, "claude"), filepath.Join(dir, "pi")
	ship := a.Run
	if final {
		ship = a.RunFinal
	}
	res, err := ship(cfg.Sources())
	if err != nil {
		return fmt.Errorf("uploading the sessions: %w", err)
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("uploading the sessions: %d files had errors: %s", len(res.Errors), strings.Join(res.Errors[:min(3, len(res.Errors))], "; "))
	}
	vr, err := a.Verify(o, cfg.Sources())
	if err != nil {
		return fmt.Errorf("verifying the upload: %w", err)
	}
	if vr.Mismatched > 0 {
		return fmt.Errorf("verifying the upload: %d files differ from the archive: %s", vr.Mismatched, strings.Join(vr.Failures[:min(3, len(vr.Failures))], "; "))
	}
	if final && (vr.Missing > 0 || vr.Behind > 0) {
		return fmt.Errorf("verifying the upload: %d files not archived and %d not fully archived", vr.Missing, vr.Behind)
	}
	e.printf("  sessions: %d files pulled, %d new bytes uploaded to host %s, %d verified by sha256\n", files, res.RawBytes, cfg.Host, vr.Matched)
	return nil
}

// ---- worker archive ----

func (e *env) archiveOne(ctx context.Context, name string) error {
	inst, err := e.prov().Describe(ctx, name)
	if err != nil {
		return err
	}
	if inst == nil {
		return fmt.Errorf("no instance %s (%s)", name, e.Cfg.Provider)
	}
	if err := checkOwned(inst); err != nil {
		return err
	}
	if inst.State != stateRunning {
		return fmt.Errorf("%s is %s: its sessions are on its boot disk, so run `worker start %s` first", name, inst.State, name)
	}
	return e.saveSessions(ctx, inst, false)
}

// archiveAll saves every running worker. It never starts a stopped one: its boot
// disk still holds the sessions, and starting it would cost money.
func (e *env) archiveAll(ctx context.Context) error {
	insts, err := e.prov().Instances(ctx)
	if err != nil {
		return err
	}
	var failed []string
	for _, i := range insts {
		if i.State != stateRunning {
			e.printf("%s: %s, skipped\n", i.Name, i.State)
			continue
		}
		e.printf("%s:\n", i.Name)
		if err := e.archiveOne(ctx, i.Name); err != nil {
			e.printf("  failed: %v\n", err)
			failed = append(failed, i.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not archive: %s", strings.Join(failed, ", "))
	}
	return nil
}

// ---- destroy ----

// archiveBeforeDestroy saves a worker's sessions. Any error means destroy must
// stop, and by then the VM is running with its agents restarted, or the output
// says plainly that they are not.
func (e *env) archiveBeforeDestroy(ctx context.Context, inst *Inst) error {
	// With the agents still running: if the archive or the VM cannot be reached,
	// this refuses with nothing changed.
	e.printf("  saving the sessions, agents still running\n")
	if err := e.saveSessions(ctx, inst, false); err != nil {
		return fmt.Errorf("destroy refused, nothing was changed: %w\nFix that and run it again, or pass --no-archive to lose this worker's sessions", err)
	}
	if _, err := e.sh(ctx, inst.Name, e.guest().StopServices, nil); err != nil {
		e.printf("  stopping the agents: %v (continuing: the copy is still a snapshot)\n", err)
	}
	e.printf("  saving the sessions, agents stopped\n")
	err := e.saveSessions(ctx, inst, true)
	if err == nil {
		return nil
	}
	msg := fmt.Sprintf("destroy refused: %v\nThe VM was left running and nothing was deleted. Fix that and run it again, or pass --no-archive to lose this worker's sessions", err)
	if _, rerr := e.sh(ctx, inst.Name, e.guest().StartServices, nil); rerr != nil {
		e.printf("  RESTARTING THE AGENTS FAILED: %v\n", rerr)
		return fmt.Errorf("%s\nThe agents are STOPPED: the restart failed too (%v). Start them on the VM with: systemctl --user start herdr comebye", msg, rerr)
	}
	e.printf("  agents restarted\n")
	return errors.New(msg)
}
