package archive

import (
	"fmt"
	"regexp"
	"strings"
)

// Produced is something a session made, tied to the message that made it:
// the commit, Page, PR, issue, comment, release or pushed image. The index
// keeps one row per artifact, so that "where did this come from?" and "what
// did this session produce?" are queries. Everything here is read from the
// transcript; nothing is written into a repository.
type Produced struct {
	Type   string // commit | page | pr | issue | comment | release | image
	Ref    string // sha | pg_id | owner/repo#N | owner/repo@tag (or tag) | sha256 digest
	URL    string // where it lives, when the output said (never for a Page)
	Repo   string // owner/repo, when known
	Branch string
	Title  string
	Rel    string // a commit's made | pushed | seen
	Detail string // version of a Page, the tag of an image, and the like
	// The message that made it: its number, subagent thread, tool call and time.
	Seq    int
	Sub    string
	ToolID string
	TS     string
}

// at says which message a finding came from. Seq is -1 when it came from
// none (a message with no text is not kept).
type at struct {
	seq     int
	sub, id string
	ts      string
}

func (a at) none() bool { return a.seq < 0 }

var (
	cmdPRCreate    = regexp.MustCompile(`\bgh\s+pr\s+create\b`)
	cmdIssueCreate = regexp.MustCompile(`\bgh\s+issue\s+create\b`)
	cmdComment     = regexp.MustCompile(`\bgh\s+(?:pr|issue)\s+comment\b`)
	cmdRelease     = regexp.MustCompile(`\bgh\s+release\s+(?:create|edit)\b`)
	cmdImagePush   = regexp.MustCompile(`(?:--push\b|\b(?:docker|podman|skopeo|buildah)\s+(?:image\s+)?push\b)`)

	// An artifact's URL is the only thing on its line: gh prints it that way, and a URL
	// inside a sentence or a notes file is only mentioned.
	urlPR      = regexp.MustCompile(`(?m)^[ \t]*https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/pull/(\d+)[ \t]*$`)
	urlIssue   = regexp.MustCompile(`(?m)^[ \t]*https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/issues/(\d+)[ \t]*$`)
	urlComment = regexp.MustCompile(`(?m)^[ \t]*(https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/(?:pull|issues)/(\d+)#(?:issuecomment|discussion_r|pullrequestreview)-?\d+)[ \t]*$`)
	urlRelease = regexp.MustCompile(`(?m)^[ \t]*https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/releases/tag/([^\s/]+)[ \t]*$`)

	// comebye publish: "Published pg_… (html, 29.7 KiB)." and "Updated pg_… to version 2".
	pagePublished = regexp.MustCompile(`(?m)^(?:Published (pg_[0-9a-f]{10}) \(|Updated (pg_[0-9a-f]{10}) to version (\d+))`)

	// podman push ends with the manifest's digest alone on a line; docker push says
	// "TAG: digest: sha256:… size: N". "Copying config sha256:…" is the config's, not the image's.
	podmanDigest = regexp.MustCompile(`(?m)^(sha256:[0-9a-f]{64})[ \t]*$`)
	dockerDigest = regexp.MustCompile(`(?m)^(\S+): digest: (sha256:[0-9a-f]{64}) size: \d+`)

	flagTitle  = regexp.MustCompile(`(?:--title|\s-t)(?:\s+|=)(?:"([^"]*)"|'([^']*)'|(\S+))`)
	flagHead   = regexp.MustCompile(`--head(?:\s+|=)(\S+)`)
	flagRepo   = regexp.MustCompile(`(?:--repo|\s-R)(?:\s+|=)([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)`)
	cmdRelTag  = regexp.MustCompile(`\bgh\s+release\s+(?:create|edit)\s+(?:(?:-\S+(?:\s+\S+)?\s+)*?)([A-Za-z0-9][A-Za-z0-9._/-]*)`)
	pushTag    = regexp.MustCompile(`build\.sh\s+(v?[0-9][A-Za-z0-9._-]*)`)
	maxProduce = 40 // a session's findings from one result
)

// producedIn finds what a tool call made, from the command and its result.
// The result is trusted only for what only that command prints: a PR URL on a
// line of its own after `gh pr create`, a "Published pg_…" line, a pushed
// image's digest. A failed call is not passed in.
func (p *Parsed) producedIn(cmd, text string, a at) {
	if a.none() || len(text) < 8 && !cmdRelease.MatchString(cmd) {
		return
	}
	title := ""
	if m := flagTitle.FindStringSubmatch(cmd); m != nil {
		title = clip(strings.TrimSpace(m[1]+m[2]+m[3]), 160)
	}
	head := ""
	if m := flagHead.FindStringSubmatch(cmd); m != nil {
		head = m[1]
	}
	n := 0
	add := func(x Produced) {
		if n >= maxProduce {
			return
		}
		n++
		x.Seq, x.Sub, x.ToolID, x.TS = a.seq, a.sub, a.id, a.ts
		x.Title, x.Branch, x.Ref = maskPlain(x.Title), maskPlain(x.Branch), maskPlain(x.Ref)
		for _, o := range p.Produced { // one message can show the same thing twice
			if o.Seq == x.Seq && o.Type == x.Type && o.Ref == x.Ref && o.URL == x.URL {
				return
			}
		}
		p.Produced = append(p.Produced, x)
	}

	if cmdPRCreate.MatchString(cmd) {
		for _, m := range urlPR.FindAllStringSubmatch(text, 8) {
			add(Produced{Type: "pr", Ref: m[1] + "#" + m[2], URL: "https://github.com/" + m[1] + "/pull/" + m[2], Repo: m[1], Branch: head, Title: title})
		}
	}
	if cmdIssueCreate.MatchString(cmd) {
		for _, m := range urlIssue.FindAllStringSubmatch(text, 8) {
			add(Produced{Type: "issue", Ref: m[1] + "#" + m[2], URL: "https://github.com/" + m[1] + "/issues/" + m[2], Repo: m[1], Title: title})
		}
	}
	if cmdComment.MatchString(cmd) {
		for _, m := range urlComment.FindAllStringSubmatch(text, 8) {
			add(Produced{Type: "comment", Ref: m[2] + "#" + m[3], URL: m[1], Repo: m[2]})
		}
	}
	if cmdRelease.MatchString(cmd) {
		found := false
		for _, m := range urlRelease.FindAllStringSubmatch(text, 4) {
			found = true
			add(Produced{Type: "release", Ref: m[1] + "@" + m[2], URL: "https://github.com/" + m[1] + "/releases/tag/" + m[2], Repo: m[1]})
		}
		if !found { // gh's output is often sent to /dev/null: the command says which
			if m := cmdRelTag.FindStringSubmatch(cmd); m != nil {
				repo, ref := "", m[1]
				if r := flagRepo.FindStringSubmatch(cmd); r != nil {
					repo, ref = r[1], r[1]+"@"+m[1]
				}
				add(Produced{Type: "release", Ref: ref, Repo: repo, Title: title, Detail: "from the command"})
			}
		}
	}
	for _, m := range pagePublished.FindAllStringSubmatch(text, 8) {
		if m[1] != "" {
			add(Produced{Type: "page", Ref: m[1], Title: title, Detail: "version 1"})
		} else {
			add(Produced{Type: "page", Ref: m[2], Title: title, Detail: "version " + m[3]})
		}
	}
	if cmdImagePush.MatchString(cmd) {
		tag := ""
		if m := pushTag.FindStringSubmatch(cmd); m != nil {
			tag = m[1]
		}
		for _, m := range dockerDigest.FindAllStringSubmatch(text, 4) {
			add(Produced{Type: "image", Ref: m[2], Title: m[1], Detail: "docker push"})
		}
		if strings.Contains(text, "Writing manifest to image destination") {
			// The last bare digest is the manifest's: the one the registry now holds.
			if all := podmanDigest.FindAllStringSubmatch(text, -1); len(all) > 0 {
				add(Produced{Type: "image", Ref: all[len(all)-1][1], Title: tag, Detail: "podman push"})
			}
		}
	}
}

// producedInsert is the statement that adds one row to the produced table.
func producedInsert(host, kind, sid string, x Produced) string {
	return fmt.Sprintf("INSERT INTO produced(host,kind,session_id,seq,sub,tool_id,ts,type,ref,url,repo,branch,title,rel,patch_id,pr,detail) VALUES(%s,%s,%s,%d,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,'','',%s);",
		q(host), q(kind), q(sid), x.Seq, q(x.Sub), q(x.ToolID), q(x.TS), q(x.Type), q(x.Ref), q(x.URL), q(x.Repo), q(x.Branch), q(x.Title), q(x.Rel), q(x.Detail))
}
