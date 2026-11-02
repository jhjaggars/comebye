package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/archive"
)

// archive and recall work on this host's files and index; they do not need
// the Comebye server.

func archiveFlags(e *Env, fs *flag.FlagSet) *archive.Config {
	c := &archive.Config{}
	env := func(k string) string {
		if e.Getenv == nil {
			return ""
		}
		return e.Getenv(k)
	}
	fs.StringVar(&c.Dir, "dir", env("COMEBYE_ARCHIVE_DIR"), "archive directory (COMEBYE_ARCHIVE_DIR)")
	fs.StringVar(&c.Recipient, "recipient", env("COMEBYE_ARCHIVE_RECIPIENT"), "public key, or a file holding it (COMEBYE_ARCHIVE_RECIPIENT)")
	fs.StringVar(&c.Identity, "identity", env("COMEBYE_ARCHIVE_IDENTITY"), "private key file, needed to read the archive back (COMEBYE_ARCHIVE_IDENTITY)")
	fs.StringVar(&c.ClaudeDir, "claude-dir", env("COMEBYE_ARCHIVE_CLAUDE_DIR"), "Claude Code projects dir (default ~/.claude/projects)")
	fs.StringVar(&c.PiDir, "pi-dir", env("COMEBYE_ARCHIVE_PI_DIR"), "pi sessions dir (default ~/.pi/agent/sessions)")
	fs.StringVar(&c.Index, "index", env("COMEBYE_RECALL_INDEX"), "index file (default ~/.local/state/comebye/recall/index.db)")
	fs.StringVar(&c.Host, "host", env("COMEBYE_HOST_NAME"), "this host's name in archive keys (default the short hostname)")
	return c
}

func cmdArchive(e *Env, args []string) error {
	if len(args) == 0 {
		return usageErr("archive needs a subcommand: keygen, run, verify, status, index, restore, delete, summary")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("archive "+sub, flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	cfg := archiveFlags(e, fs)
	switch sub {
	case "keygen":
		out := fs.String("out", "", "directory to write identity.key and recipient.pub to (required)")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) > 0 || *out == "" {
			return usageErr("usage: comebye archive keygen --out DIR")
		}
		rcpt, ident, err := archive.GenerateKey()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(*out, 0o700); err != nil {
			return err
		}
		idf, pubf := filepath.Join(*out, "identity.key"), filepath.Join(*out, "recipient.pub")
		if err := writeNew(idf, "# comebye archive identity: whoever holds this can read the archive. Keep it off the archiving host.\n# recipient: "+rcpt+"\n"+ident+"\n", 0o600); err != nil {
			return err
		}
		if err := writeNew(pubf, rcpt+"\n", 0o644); err != nil {
			return err
		}
		if *asJSON {
			return jsonOut(e, map[string]string{"identity": idf, "recipient_file": pubf, "recipient": rcpt})
		}
		fmt.Fprintf(e.Stdout, "identity (secret): %s\nrecipient (public): %s\n%s\n", idf, pubf, rcpt)
		return nil
	case "run", "verify", "status", "index", "restore", "delete", "summary":
	default:
		return usageErr("unknown archive subcommand %q", sub)
	}
	var ledger, to, by *string
	var yes *bool
	switch sub {
	case "index":
		ledger = fs.String("ledger", "auto", "handoff links: FILE or - (the output of `comebye ledger --json`), auto (run it), or none")
	case "restore":
		to = fs.String("to", "", "directory to restore the session's files into (required)")
	case "delete":
		yes = fs.Bool("yes", false, "really delete (without it, only say what would be deleted)")
	case "summary":
		by = fs.String("by", "", "who wrote the summary")
	}
	pos, err := parse(fs, rest)
	if err != nil {
		return err
	}
	a, err := cfg.Open()
	if err != nil {
		return err
	}
	switch sub {
	case "run":
		if len(pos) > 0 {
			return usageErr("unexpected argument %q", pos[0])
		}
		t0 := time.Now()
		res, err := a.Run(cfg.Sources())
		if err != nil {
			return err
		}
		if *asJSON {
			return jsonOut(e, map[string]any{"result": res, "seconds": time.Since(t0).Seconds()})
		}
		fmt.Fprintf(e.Stdout, "%d files looked at, %d updated, %d segments written, %s copied, %s stored (%.1fs)\n",
			res.Files, res.Updated, res.Segments, human(res.RawBytes), human(res.StoredBytes), time.Since(t0).Seconds())
		if res.Tombstoned+res.Shrunk+res.Conflicts > 0 {
			fmt.Fprintf(e.Stdout, "skipped: %d deleted-on-request, %d smaller than archived, %d conflicts\n", res.Tombstoned, res.Shrunk, res.Conflicts)
		}
		for _, m := range res.Errors {
			fmt.Fprintln(e.Stderr, "error:", m)
		}
		if len(res.Errors) > 0 {
			return fmt.Errorf("%d files had errors", len(res.Errors))
		}
		return nil
	case "verify":
		o, err := cfg.OpenIdentity()
		if err != nil {
			return err
		}
		t0 := time.Now()
		vr, err := a.Verify(o, cfg.Sources())
		if err != nil {
			return err
		}
		if *asJSON {
			return jsonOut(e, map[string]any{"result": vr, "seconds": time.Since(t0).Seconds()})
		}
		fmt.Fprintf(e.Stdout, "%d files: %d match their archive by sha256 (%s compared; %d have newer bytes not yet archived), %d not archived, %d differ (%.1fs)\n",
			vr.Files, vr.Matched, human(vr.Bytes), vr.Behind, vr.Missing, vr.Mismatched, time.Since(t0).Seconds())
		for _, f := range vr.Failures {
			fmt.Fprintln(e.Stderr, "mismatch:", f)
		}
		if vr.Mismatched > 0 {
			return fmt.Errorf("%d files differ from their archive", vr.Mismatched)
		}
		return nil
	case "status":
		ss, err := a.Sessions()
		if err != nil {
			return err
		}
		var raw, segs, stored int64
		byKind := map[string]int{}
		for _, s := range ss {
			raw += s.Bytes
			segs += int64(s.Segments)
			byKind[s.Kind]++
		}
		if sz, ok := a.B.(interface{ Size(string) (int64, error) }); ok {
			keys, _ := a.B.List("v1/")
			for _, k := range keys {
				if n, err := sz.Size(k); err == nil {
					stored += n
				}
			}
		}
		if *asJSON {
			return jsonOut(e, map[string]any{"host": a.Host, "sessions": len(ss), "by_kind": byKind, "segments": segs, "raw_bytes": raw, "stored_bytes": stored})
		}
		ratio := 0.0
		if stored > 0 {
			ratio = float64(raw) / float64(stored)
		}
		fmt.Fprintf(e.Stdout, "host %s: %d sessions %v, %d segments, %s raw, %s stored (%.1fx)\n", a.Host, len(ss), byKind, segs, human(raw), human(stored), ratio)
		return nil
	case "index":
		o, err := cfg.OpenIdentity()
		if err != nil {
			return err
		}
		var links []archive.Link
		switch *ledger {
		case "none":
		case "auto":
			exe, _ := os.Executable()
			var b bytes.Buffer
			cmd := exec.Command(exe, "ledger", "--json")
			cmd.Stdout = &b
			if err := cmd.Run(); err != nil {
				fmt.Fprintln(e.Stderr, "note: could not read the ledger (", err, "); indexing without handoff links")
			} else if links, err = archive.LoadLinks(&b); err != nil {
				fmt.Fprintln(e.Stderr, "note: bad ledger output:", err)
			}
		default:
			var r io.Reader = e.Stdin
			if *ledger != "-" {
				f, err := os.Open(*ledger)
				if err != nil {
					return err
				}
				defer f.Close()
				r = f
			}
			if links, err = archive.LoadLinks(r); err != nil {
				return err
			}
		}
		ix := &archive.Index{Path: cfg.IndexPath()}
		t0 := time.Now()
		st, err := ix.Sync(a, o, links)
		if err != nil {
			return err
		}
		if *asJSON {
			return jsonOut(e, map[string]any{"result": st, "index": ix.Path, "links": len(links), "seconds": time.Since(t0).Seconds()})
		}
		fmt.Fprintf(e.Stdout, "%d sessions: %d indexed (%d messages), %d unchanged, %d removed; %d handoff links read; %s (%.1fs)\n",
			st.Sessions, st.Indexed, st.Messages, st.Unchanged, st.Removed, len(links), ix.Path, time.Since(t0).Seconds())
		for _, m := range st.Errors {
			fmt.Fprintln(e.Stderr, "error:", m)
		}
		return nil
	case "restore":
		if len(pos) != 1 || *to == "" {
			return usageErr("usage: comebye archive restore SESSION --to DIR")
		}
		o, err := cfg.OpenIdentity()
		if err != nil {
			return err
		}
		ref, err := a.FindSession(pos[0])
		if err != nil {
			return err
		}
		var total int64
		for _, s := range ref.Streams {
			dst := filepath.Join(*to, filepath.FromSlash(s))
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			n, err := archive.StreamTo(f, a.B, o, a.Host, ref.Kind, s)
			f.Close()
			if err != nil {
				return err
			}
			total += n
		}
		fmt.Fprintf(e.Stdout, "restored %s %s: %d files, %s, into %s\n", ref.Kind, ref.SID, len(ref.Streams), human(total), *to)
		return nil
	case "delete":
		if len(pos) != 1 {
			return usageErr("usage: comebye archive delete SESSION [--yes]")
		}
		ref, err := a.FindSession(pos[0])
		if err != nil {
			return err
		}
		if !*yes {
			fmt.Fprintf(e.Stdout, "would delete %s %s: %d files, %d segments, %s raw. The source files are not touched. Run again with --yes.\n", ref.Kind, ref.SID, len(ref.Streams), ref.Segments, human(ref.Bytes))
			return nil
		}
		n, err := a.DeleteSession(ref.Kind, ref.Slug, ref.SID)
		if err != nil {
			return err
		}
		if _, err := os.Stat(cfg.IndexPath()); err == nil {
			if err := (&archive.Index{Path: cfg.IndexPath()}).DeleteSession(a.Host, ref.Kind, ref.SID); err != nil {
				return err
			}
		}
		fmt.Fprintf(e.Stdout, "deleted %s %s: %d segments, and its index rows. A tombstone keeps it from being archived again while the source files exist.\n", ref.Kind, ref.SID, n)
		return nil
	case "summary":
		if len(pos) != 2 {
			return usageErr("usage: comebye archive summary SESSION TEXT|-")
		}
		ref, err := a.FindSession(pos[0])
		if err != nil {
			return err
		}
		text := pos[1]
		if text == "-" {
			b, err := io.ReadAll(e.Stdin)
			if err != nil {
				return err
			}
			text = string(b)
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return usageErr("empty summary")
		}
		if err := a.SetSummary(ref.Kind, ref.SID, text, *by); err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "summary stored for %s %s (shown after the next `comebye archive index`)\n", ref.Kind, ref.SID)
		return nil
	}
	return nil
}

func writeNew(path, content string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func human(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB", "TB"} {
		if f < 1024 || u == "TB" {
			if u == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1024
	}
	return ""
}

func cmdRecall(e *Env, args []string) error {
	if len(args) == 0 {
		return usageErr("recall needs a subcommand: list, search, show, commit")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("recall "+sub, flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	envIdx := ""
	if e.Getenv != nil {
		envIdx = e.Getenv("COMEBYE_RECALL_INDEX")
	}
	idx := fs.String("index", envIdx, "index file (COMEBYE_RECALL_INDEX; default ~/.local/state/comebye/recall/index.db)")
	var f archive.Filter
	fs.StringVar(&f.Since, "since", "", "sessions active on or after this date (2026-10-01)")
	fs.StringVar(&f.Until, "until", "", "sessions started before this date (2026-11-01)")
	fs.StringVar(&f.Host, "host", "", "only this host")
	fs.StringVar(&f.Kind, "kind", "", "only this agent kind (claude, pi)")
	fs.StringVar(&f.Project, "project", "", "repo, directory, branch or title contains this")
	fs.StringVar(&f.Branch, "branch", "", "exactly this branch (the session's or its handoff's)")
	fs.StringVar(&f.Ledger, "ledger", "", "a handoff id")
	fs.IntVar(&f.Limit, "limit", 0, "most rows (default 30; search 15)")
	text := fs.Int("text", 0, "show: also print the first N indexed messages")
	switch sub {
	case "list", "search", "show", "commit":
	default:
		return usageErr("unknown recall subcommand %q", sub)
	}
	pos, err := parse(fs, rest)
	if err != nil {
		return err
	}
	path := *idx
	if path == "" {
		path = archive.Config{}.IndexPath()
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no index at %s: run `comebye archive index` first", path)
	}
	ix := &archive.Index{Path: path}
	switch sub {
	case "list", "search", "commit":
		var rows []map[string]any
		switch sub {
		case "list":
			rows, err = ix.List(f)
		case "search":
			if len(pos) == 0 {
				return usageErr("usage: comebye recall search WORDS... [filters]")
			}
			rows, err = ix.Search(strings.Join(pos, " "), f)
		case "commit":
			if len(pos) != 1 {
				return usageErr("usage: comebye recall commit SHA")
			}
			rows, err = ix.ByCommit(pos[0])
		}
		if err != nil {
			return err
		}
		if *asJSON {
			return jsonOut(e, rows)
		}
		if len(rows) == 0 {
			fmt.Fprintln(e.Stdout, "nothing found")
		}
		for _, r := range rows {
			printSession(e, r)
		}
		return nil
	case "show":
		if len(pos) != 1 {
			return usageErr("usage: comebye recall show SESSION [--text N]")
		}
		s, cs, err := ix.Show(pos[0])
		if err != nil {
			return err
		}
		var msgs []map[string]any
		if *text > 0 {
			msgs, err = ix.Text(str(s["host"]), str(s["kind"]), str(s["id"]), *text)
			if err != nil {
				return err
			}
		}
		if *asJSON {
			return jsonOut(e, map[string]any{"session": s, "commits": cs, "messages": msgs})
		}
		printSession(e, s)
		for _, k := range []string{"cwd", "first_prompt", "brief", "summary", "plan_id", "ref", "ledger_agent", "version"} {
			if v := str(s[k]); v != "" {
				fmt.Fprintf(e.Stdout, "    %-12s %s\n", k, v)
			}
		}
		fmt.Fprintf(e.Stdout, "    %-12s %v messages, %v from subagents, %v bytes archived in %v files\n", "size", s["n_msgs"], s["n_sub"], s["n_bytes"], s["n_streams"])
		for _, c := range cs {
			fmt.Fprintf(e.Stdout, "    commit %-8s %-7s %s %s\n", str(c["sha"])[:min(8, len(str(c["sha"])))], str(c["rel"]), str(c["branch"]), str(c["subject"]))
		}
		for _, m := range msgs {
			who := str(m["role"])
			if str(m["sub"]) != "" {
				who += "(sub " + str(m["sub"]) + ")"
			}
			fmt.Fprintf(e.Stdout, "  [%s] %s: %s\n", str(m["seq"]), who, oneLine(str(m["text"]), 300))
		}
		return nil
	}
	return nil
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case float64:
		return fmt.Sprintf("%.0f", x)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

func shortID(kind, id string) string {
	if kind == "pi" {
		if _, after, ok := strings.Cut(id, "_"); ok {
			id = after
		}
	}
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

func printSession(e *Env, r map[string]any) {
	when := str(r["started"])
	if len(when) >= 16 {
		when = strings.Replace(when[:16], "T", " ", 1)
	}
	where := str(r["repo"])
	if b := str(r["branch"]); b != "" {
		where += "@" + b
	}
	title := str(r["title"])
	if title == "" {
		title = oneLine(str(r["first_prompt"]), 70)
	}
	extra := ""
	if v := str(r["ledger_id"]); v != "" {
		extra += " [handoff " + v + "]"
	}
	if v := str(r["ref"]); v != "" {
		extra += " [" + v + "]"
	}
	fmt.Fprintf(e.Stdout, "%s  %-6s %s  %s  %s%s\n", when, str(r["kind"]), shortID(str(r["kind"]), str(r["id"])), where, oneLine(title, 80), extra)
	if sn := str(r["snippet"]); sn != "" {
		fmt.Fprintf(e.Stdout, "    %s: %s\n", str(r["role"]), oneLine(sn, 200))
	}
	if sm := str(r["summary"]); sm != "" && str(r["snippet"]) == "" {
		fmt.Fprintf(e.Stdout, "    summary: %s\n", oneLine(sm, 200))
	}
	if sh := str(r["sha"]); sh != "" {
		fmt.Fprintf(e.Stdout, "    commit %s (%s) %s\n", sh[:min(8, len(sh))], str(r["rel"]), str(r["subject"]))
	}
}

var _ = json.Marshal
