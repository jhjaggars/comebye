package archive

import (
	"strings"
	"testing"
)

// Made up, and built at run time so that no scanner mistakes them for real ones.
var (
	fakeGH     = "ghp_" + strings.Repeat("a1", 18)
	fakeBearer = "Tok3n" + strings.Repeat("abcDEF12", 4)
	fakeTS     = "tskey-auth-" + strings.Repeat("kQ7", 8)
	fakeAWS    = "AKIA" + strings.Repeat("ABCD2345", 2)
	fakeAnt    = "sk-ant-" + strings.Repeat("api03x", 6)
	fakeAge    = "AGE-SECRET-KEY-1" + strings.Repeat("QPZRY9X8", 6)
	fakeJWT    = "eyJhbGciOiJI" + "." + "eyJzdWIiOiIx" + "." + "c2lnbmF0dXJl"
	fakePEM    = "-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat("b3BlbnNzaC1rZXktdjEAAAAA\n", 8) + "-----END OPENSSH PRIVATE KEY-----"
)

func TestMaskHidesSecretsAndKeepsLabels(t *testing.T) {
	for _, c := range []struct{ in, gone, keep string }{
		{"token " + fakeGH + " end", fakeGH, "end"},
		{"Authorization: Bearer " + fakeBearer, fakeBearer, "Authorization: Bearer " + MaskMark},
		{"key=" + fakeTS, fakeTS, "key="},
		{"aws " + fakeAWS + " ok", fakeAWS, "ok"},
		{"x " + fakeAnt, fakeAnt, "x "},
		{fakeAge, fakeAge, MaskMark},
		{"jwt " + fakeJWT, fakeJWT, "jwt"},
		{"before\n" + fakePEM + "\nafter", "b3BlbnNzaC1rZXk", "after"},
		{`password=hunter2hunter2 next`, "hunter2hunter2", "password=" + MaskMark},
		{`"secret_key": "wJalrXUtnFEMIK7MDENG"`, "wJalrXUtnFEMIK7MDENG", `"secret_key": "` + MaskMark},
		{"https://user:s3cretpass@example.test/x", "s3cretpass", "https://user:" + MaskMark + "@example.test/x"},
	} {
		out, hit := Mask(c.in)
		if !hit || strings.Contains(out, c.gone) || !strings.Contains(out, c.keep) {
			t.Errorf("Mask(%q) = %q, %v", c.in, out, hit)
		}
	}
}

func TestMaskLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{
		"commit 1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b",
		"session 33333333-3333-4333-8333-333333333333",
		"Bearer authentication is required",
		"Bearer tokens expire",
		`password=${DB_PASSWORD} and api_key=<your key> and token: string`,
		"secret_key = os.environ",
		"the password: required",
		"http://localhost:8741/api/agents",
		"ssh://git@github.com/x/y.git",
		"sk-learn and task-force",
		"",
	} {
		if out, hit := Mask(s); hit || out != s {
			t.Errorf("Mask(%q) = %q, %v", s, out, hit)
		}
	}
}

// A value that crosses the point where text is clipped is masked whole, not cut.
func TestClippingCannotLeaveHalfASecret(t *testing.T) {
	head, hit := maskHead(strings.Repeat("x", maxMsgText-10)+" "+fakeGH+" "+strings.Repeat("y", 40000), maxMsgText)
	if !hit || strings.Contains(head, "ghp_") || strings.Contains(head, "a1a1") {
		t.Errorf("head kept part of it (%v): %q", hit, head[len(head)-40:])
	}
	out, hit, clipped := maskEnds(strings.Repeat("x", 30000)+" "+fakeGH+" "+strings.Repeat("y", maxResultText-5), maxResultText)
	if !hit || !clipped || strings.Contains(out, "a1a1") {
		t.Errorf("tail kept part of it (%v %v)", hit, clipped)
	}
	// A key whose start is before the kept tail.
	_, hit, _ = maskEnds(strings.Repeat("x", 30000)+"\n"+fakePEM+"\nend", 100)
	if !hit {
		t.Error("a key's end in the tail window was not recognised")
	}
}

func TestLongOutputKeepsBothEnds(t *testing.T) {
	long := "HEADWORD\n" + strings.Repeat("filler line\n", 2000) + "TAILWORD: exit status 1"
	p := &Parsed{}
	p.addResult("t", long, "", "id1", true)
	m := p.Msgs[0]
	if !strings.Contains(m.Text, "HEADWORD") || !strings.Contains(m.Text, "TAILWORD: exit status 1") || !strings.Contains(m.Text, "bytes left out") {
		t.Errorf("text = %q", m.Text[:50])
	}
	if m.FullLen != len(long) || !m.IsErr || m.ToolID != "id1" {
		t.Errorf("%+v", m)
	}
	if len(m.Text) > 2*maxResultText+60 {
		t.Errorf("kept %d bytes", len(m.Text))
	}
	p = &Parsed{}
	p.addResult("t", "short", "", "", false)
	if p.Msgs[0].FullLen != 0 || p.Msgs[0].Text != "short" {
		t.Errorf("%+v", p.Msgs[0])
	}
}
