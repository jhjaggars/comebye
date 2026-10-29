package fleet

import (
	"strings"
	"testing"
)

func TestParsePeers(t *testing.T) {
	got, err := ParsePeers(" kube=https://vm.tail1234.ts.net , https://Box.tail1234.ts.net:8443/ ,", ParseOptions{LocalName: "desk"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d peers", len(got))
	}
	if got[0].Name != "kube" || len(got[0].URLs) != 1 || got[0].URLs[0].String() != "https://vm.tail1234.ts.net" {
		t.Errorf("peer 0 = %s %s", got[0].Name, got[0].Addrs())
	}
	// Name defaults to the first DNS label; the URL is normalized.
	if got[1].Name != "box" || got[1].URLs[0].String() != "https://box.tail1234.ts.net:8443" {
		t.Errorf("peer 1 = %s %s", got[1].Name, got[1].Addrs())
	}
	if peers, err := ParsePeers("", ParseOptions{}); err != nil || len(peers) != 0 {
		t.Errorf("empty spec: %v %v", peers, err)
	}
}

// A peer may list several addresses (a MagicDNS name plus a tailnet IP,
// or two names): they are kept in order, and every one must pass the
// same checks as a single address.
func TestParsePeersMultipleAddresses(t *testing.T) {
	got, err := ParsePeers("kube=https://kube.tail1234.ts.net|https://100.101.102.103", ParseOptions{LocalName: "desk"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].URLs) != 2 ||
		got[0].URLs[0].String() != "https://kube.tail1234.ts.net" ||
		got[0].URLs[1].String() != "https://100.101.102.103" {
		t.Fatalf("peer = %+v", got[0])
	}
	if s := got[0].Addrs(); s != "https://kube.tail1234.ts.net https://100.101.102.103" {
		t.Errorf("Addrs() = %q", s)
	}
	// Whitespace around an address is fine; a trailing | is not.
	if got, err := ParsePeers("k=https://a.ts.net | https://b.ts.net", ParseOptions{}); err != nil || len(got[0].URLs) != 2 {
		t.Fatalf("spaced addresses: %+v %v", got, err)
	}
	for spec, want := range map[string]string{
		"k=https://a.ts.net|":                 "empty address",
		"k=https://a.ts.net||https://b.ts":    "empty address",
		"k=https://a.ts.net|http://x.ts":      "https",
		"k=https://a.ts.net|https://a.ts.net": "duplicate URL",
		"k=https://a.ts.net|ftp://b.ts":       "scheme",
	} {
		if _, err := ParsePeers(spec, ParseOptions{LocalName: "desk"}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want it to mention %q", spec, err, want)
		}
	}
}

func TestParsePeersRejects(t *testing.T) {
	cases := map[string]string{
		"http://box.ts.net":                     "https",
		"ftp://box.ts.net":                      "scheme",
		"https://box.ts.net/api":                "path",
		"https://box.ts.net?x=1":                "query",
		"https://box.ts.net#frag":               "fragment",
		"https://user:pw@box.ts.net":            "credentials",
		"box.ts.net":                            "absolute",
		"Bad_Name=https://box.ts.net":           "DNS label",
		"desk=https://desk.ts.net":              "local host",
		"a=https://a.ts.net,a=https://b.ts.net": "duplicate name",
		"a=https://a.ts.net,b=https://a.ts.net": "duplicate URL",
		"http://127.0.0.1:8742":                 "https", // loopback http only in test mode
	}
	for spec, want := range cases {
		_, err := ParsePeers(spec, ParseOptions{LocalName: "desk"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want it to mention %q", spec, err, want)
		}
	}
}

func TestParsePeersHTTPLoopbackTestMode(t *testing.T) {
	opt := ParseOptions{AllowHTTPLoopback: true}
	got, err := ParsePeers("peer=http://127.0.0.1:8742,http://localhost:9", opt)
	if err != nil || len(got) != 2 || got[0].URLs[0].Scheme != "http" || got[1].Name != "localhost" {
		t.Fatalf("got %+v, %v", got, err)
	}
	// Even in test mode, http is loopback only.
	if _, err := ParsePeers("http://100.64.0.2:8741", opt); err == nil {
		t.Fatal("http to a non-loopback address must be refused")
	}
}

func TestNames(t *testing.T) {
	for s, want := range map[string]bool{"box": true, "kube-vm2": true, "-x": false, "x-": false, "Box": false, "a.b": false, "": false, strings.Repeat("a", 64): false} {
		if ValidName(s) != want {
			t.Errorf("ValidName(%q) = %v", s, !want)
		}
	}
	for in, want := range map[string]string{"Desk.local": "desk", "my_box": "my-box", "___": ""} {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	if !ValidName(DefaultHostName()) {
		t.Errorf("DefaultHostName() = %q is not valid", DefaultHostName())
	}
}

func TestValidTarget(t *testing.T) {
	for s, want := range map[string]bool{"wS:p1": true, "my agent": true, "a/b": true, "": false, ".": false, "..": false, "a\nb": false, strings.Repeat("x", 257): false} {
		if ValidTarget(s) != want {
			t.Errorf("ValidTarget(%q) = %v", s, !want)
		}
	}
}
