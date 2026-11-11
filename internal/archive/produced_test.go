package archive

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The output shapes below are the real ones from agents' transcripts (gh, comebye
// publish, podman push through build.sh, docker push), with the names kept.

func tuLine(id, cmd string) string {
	in, _ := json.Marshal(map[string]string{"command": cmd})
	return `{"type":"assistant","timestamp":"2026-11-11T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":` + string(in) + `}]}}` + "\n"
}

func trLine(id, out string, isErr bool) string {
	o, _ := json.Marshal(out)
	return fmt.Sprintf(`{"type":"user","timestamp":"2026-11-11T10:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%s,"is_error":%v}]}}`+"\n", id, o, isErr)
}

func parseSession(t *testing.T, lines ...string) *Parsed {
	t.Helper()
	p := &Parsed{}
	if err := p.ParseClaude(strings.NewReader(strings.Join(lines, "")), ""); err != nil {
		t.Fatal(err)
	}
	return p
}

func find(p *Parsed, typ, ref string) *Produced {
	for i := range p.Produced {
		if p.Produced[i].Type == typ && p.Produced[i].Ref == ref {
			return &p.Produced[i]
		}
	}
	return nil
}

func TestProducedGitHubOutput(t *testing.T) {
	p := parseSession(t,
		// gh pr create after a push in one command: git's "remote:" lines are not the PR.
		tuLine("a", `cd /home/jhjaggars/code/homelab && git push -u origin feat/openclaw-upgrade && gh pr create --base main --head feat/openclaw-upgrade --title "Upgrade OpenClaw" --body-file b.md`),
		trLine("a", "remote: \nremote: Create a pull request for 'feat/openclaw-upgrade' on GitHub by visiting:\nremote:      https://github.com/jhjaggars/homelab/pull/new/feat/openclaw-upgrade\nremote: \nhttps://github.com/jhjaggars/homelab/pull/330\nShell cwd was reset to /home/jhjaggars/code/homelab\n", false),
		tuLine("b", `gh issue create --title "Passkey-signed approvals for sensitive user actions" --body-file /tmp/x/issue.md`),
		trLine("b", "https://github.com/jhjaggars/comebye/issues/3\n", false),
		// Two comments from one command.
		tuLine("c", `gh pr comment 3899 -R openclaw/openclaw-enterprise --body-file a.md; gh pr comment 4025 -R openclaw/openclaw-enterprise --body-file b.md`),
		trLine("c", "https://github.com/openclaw/openclaw-enterprise/pull/3899#issuecomment-6592173054\nhttps://github.com/openclaw/openclaw-enterprise/pull/4025#issuecomment-6592173407\n", false),
		// A release edit whose output went to /dev/null: the command says which.
		tuLine("d", `cd /tmp/s && gh release edit v0.2.1 -R jhjaggars/comebye --notes-file notes.md >/dev/null && echo done`),
		trLine("d", "done\n", false),
		// Not made: a failed call, a URL only mentioned in prose, a PR that was viewed, and
		// a brief that merely contains the command's name.
		tuLine("e", `gh issue create --title "Dup" --body-file x.md`),
		trLine("e", "https://github.com/jhjaggars/comebye/issues/99\n", true),
		tuLine("f", `gh pr view 12 --json url`),
		trLine("f", "https://github.com/jhjaggars/comebye/pull/12\n", false),
		tuLine("g", "cat > brief.md <<'EOF'\nOpen it with gh pr create and read https://github.com/jhjaggars/comebye/pull/7 first\nEOF\n"),
		trLine("g", "See https://github.com/jhjaggars/comebye/pull/7 for the plan.\n", false),
	)
	pr := find(p, "pr", "jhjaggars/homelab#330")
	if pr == nil || pr.URL != "https://github.com/jhjaggars/homelab/pull/330" || pr.Branch != "feat/openclaw-upgrade" || pr.Title != "Upgrade OpenClaw" || pr.Repo != "jhjaggars/homelab" {
		t.Fatalf("pr: %+v", pr)
	}
	if pr.Seq != 1 || pr.ToolID != "a" { // the result is message 1: the call is 0
		t.Errorf("the PR is tied to the result message: seq %d tool %q", pr.Seq, pr.ToolID)
	}
	if is := find(p, "issue", "jhjaggars/comebye#3"); is == nil || is.Title != "Passkey-signed approvals for sensitive user actions" || is.Seq != 3 {
		t.Errorf("issue: %+v", is)
	}
	if c := find(p, "comment", "openclaw/openclaw-enterprise#3899"); c == nil || !strings.HasSuffix(c.URL, "#issuecomment-6592173054") {
		t.Errorf("comment: %+v", c)
	}
	if find(p, "comment", "openclaw/openclaw-enterprise#4025") == nil {
		t.Error("the second comment of one command was lost")
	}
	rel := find(p, "release", "jhjaggars/comebye@v0.2.1")
	if rel == nil || rel.Detail != "from the command" || rel.Repo != "jhjaggars/comebye" {
		t.Errorf("release: %+v", rel)
	}
	for _, x := range p.Produced {
		if x.Ref == "jhjaggars/comebye#99" || x.Ref == "jhjaggars/comebye#12" || x.Ref == "jhjaggars/comebye#7" {
			t.Errorf("not made by this session: %+v", x)
		}
	}
	if len(p.Produced) != 5 {
		t.Errorf("want 5 findings, got %d: %+v", len(p.Produced), p.Produced)
	}
}

func TestProducedPages(t *testing.T) {
	p := parseSession(t,
		tuLine("a", `comebye publish page.html --key design-artifact-provenance-mockups`),
		trLine("a", "https://jjaggars-kubevirt.tail4a1fd.ts.net/p/pg_867b4272a4\nPublished pg_867b4272a4 (html, 29.7 KiB). Update it with `comebye publish FILE --update pg_867b4272a4`.\n", false),
		tuLine("b", `comebye publish page.html --key design-artifact-provenance-mockups`),
		trLine("b", "https://jjaggars-kubevirt.tail4a1fd.ts.net/p/pg_867b4272a4\nUpdated pg_867b4272a4 to version 2 (37.2 KiB).\n", false),
		// A briefing is not a Page.
		tuLine("c", `comebye briefing publish b.md`),
		trLine("c", "Published the briefing; it shows at the top of the app.\n", false),
	)
	if len(p.Produced) != 2 || p.Produced[0].Type != "page" || p.Produced[0].Ref != "pg_867b4272a4" {
		t.Fatalf("%+v", p.Produced)
	}
	if p.Produced[0].Detail != "version 1" || p.Produced[1].Detail != "version 2" {
		t.Errorf("versions: %+v", p.Produced)
	}
	if p.Produced[0].URL != "" {
		t.Error("a Page's link is built by the app, not stored")
	}
	if p.Produced[0].Seq != 1 || p.Produced[1].Seq != 3 {
		t.Errorf("seq %d %d", p.Produced[0].Seq, p.Produced[1].Seq)
	}
}

func TestProducedImages(t *testing.T) {
	p := parseSession(t,
		// build.sh --push: podman prints the config digest as "Copying config" and the
		// manifest's digest alone on the last line. Only the last is the image.
		tuLine("a", `cd tools/worker/openshell && ./build.sh v0.5.23 --push 2>&1 | tail -4`),
		trLine("a", "Copying config sha256:25b787cde9588c46cc6abd747a04e3655a0d38b9afa3d95446de4cd5a36bbc6d\nWriting manifest to image destination\nsha256:1421b6697b5c5667f4138f78ec69bc18cff0bd3e76dbbb24780f137c07ea688b\n", false),
		tuLine("b", `docker push registry.example/app:v1`),
		trLine("b", "The push refers to repository [registry.example/app]\nv1: digest: sha256:882e93d4c3d4a396282e9df2a10b45ebaa9eebe72b43e60057a9ebed5a0f4847 size: 1573\n", false),
		// Building with no push is not a push.
		tuLine("c", `./build.sh v0.5.24`),
		trLine("c", "Writing manifest to image destination\nsha256:37a8026da75d26b4bc6e7e075bfd592fe9ac4e4db7c9fc080edfe600954c876e\n", false),
	)
	img := find(p, "image", "sha256:1421b6697b5c5667f4138f78ec69bc18cff0bd3e76dbbb24780f137c07ea688b")
	if img == nil || img.Title != "v0.5.23" || img.Detail != "podman push" {
		t.Fatalf("podman: %+v", img)
	}
	if find(p, "image", "sha256:25b787cde9588c46cc6abd747a04e3655a0d38b9afa3d95446de4cd5a36bbc6d") != nil {
		t.Error("the config digest is not the image")
	}
	if d := find(p, "image", "sha256:882e93d4c3d4a396282e9df2a10b45ebaa9eebe72b43e60057a9ebed5a0f4847"); d == nil || d.Title != "v1" {
		t.Errorf("docker: %+v", d)
	}
	if len(p.Produced) != 2 {
		t.Errorf("%+v", p.Produced)
	}
}

// A commit is tied to the message that made it, and a later push upgrades it.
func TestCommitsKeepTheirMessage(t *testing.T) {
	p := parseSession(t,
		tuLine("a", `git commit -m "Add the failover drill notes"`),
		trLine("a", "[ops/garage-drill 9c1e4ab] Add the failover drill notes\n 1 file changed\n", false),
		tuLine("b", `git push`),
		trLine("b", "   9c1e4ab..4d02f17  ops/garage-drill -> ops/garage-drill\n", false),
		tuLine("c", `git log -3`),
		trLine("c", "commit 9c1e4ab0000000000000000000000000000000aa\nAuthor: x\n", false),
	)
	var made *CommitRef
	for i := range p.Commits {
		if strings.HasPrefix(p.Commits[i].SHA, "9c1e4ab") {
			made = &p.Commits[i]
		}
	}
	if made == nil || made.Rel != "made" || made.Seq != 1 || made.ToolID != "a" {
		t.Fatalf("%+v", p.Commits)
	}
}

// End to end: the index keeps produced rows, the commits view still answers, and a
// session indexed again does not double them.
func TestIndexKeepsProduced(t *testing.T) {
	r := newRig(t)
	ix := &Index{Path: r.dir + "/index.db"}
	r.hostSession("hub", sidHub1, "/home/u/code/comebye", "main", "2026-11-11T09:00:00Z", "publish the notes",
		"Published pg_8f21c0a4d7 (html, 1 KiB).\n",
		"[main 9c1e4ab] Add the notes\n")
	for i := 0; i < 2; i++ {
		if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := ix.Query("SELECT type, ref, seq, tool_id FROM produced WHERE session_id=" + q(sidHub1) + " ORDER BY seq;")
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %v", err, rows)
	}
	if rows[0]["type"] != "page" || rows[0]["ref"] != "pg_8f21c0a4d7" || rows[1]["type"] != "commit" {
		t.Errorf("%v", rows)
	}
	// The message the Page came from is that seq.
	msg, err := ix.Query("SELECT role, text FROM messages WHERE session_id=" + q(sidHub1) + " AND seq=" + fmt.Sprint(rows[0]["seq"]) + ";")
	if err != nil || len(msg) != 1 || !strings.Contains(fmt.Sprint(msg[0]["text"]), "Published pg_8f21c0a4d7") {
		t.Errorf("seq does not point at the result: %v %v", msg, err)
	}
	by, err := ix.ByCommit("9c1e4ab")
	if err != nil || len(by) != 1 {
		t.Errorf("the commits view: %v %v", by, err)
	}
}
