package cli

import (
	"strings"
	"testing"
)

const usageJSON = `{"from":1793000000,"to":1794000000,"by":"project","bucket":"day",
 "totals":{"in":1000,"out":2000,"cw":3000,"cr":4000000,"calls":50,"fresh":6000,"usd":1.5,"unpriced":200,"hit":0.99},
 "groups":[{"key":"comebye","in":1000,"out":2000,"cw":3000,"cr":4000000,"calls":50,"fresh":6000,"usd":1.5,"hit":0.99}],
 "top_handoffs":[{"handoff":"h1","name":"w1","project":"comebye","sessions":2,"fresh":6000}],
 "top_sessions":[{"host":"laharl","kind":"claude","id":"0123456789abcdef","project":"comebye","role":"worker","fresh":6000}],
 "cache":{"hit":0.99,"read_per_write":1333.3},"hosts":[{"name":"kube","err":"kube did not answer in time"}]}`

func TestUsageShowsFreshAndCacheApart(t *testing.T) {
	e, out, _, reqs := peerEnv(t, nil, map[string]answer{"GET /api/usage": {200, usageJSON}})
	if code := run(e, "usage", "--by", "project", "--since", "7d", "--project", "comebye"); code != 0 {
		t.Fatalf("exit %d %s", code, out)
	}
	for _, want := range []string{"fresh 6.0k (input 1.0k + output 2.0k + cache writes 3.0k)", "cache reads 4.00M apart", "99% of the prompt side", "$1.50+", "have no price", "! kube: kube did not answer", "PROJECT", "comebye", "Top handoffs", "h1", "Top sessions", "laharl/claude 89abcdef"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	r := (*reqs)[0]
	if r.method != "GET" || !strings.Contains(r.query, "by=project") || !strings.Contains(r.query, "since=7d") || !strings.Contains(r.query, "project=comebye") || !strings.Contains(r.query, "tz=") {
		t.Fatalf("request %+v", r)
	}
	// A coordinator reads with its agent token.
	if h := r.hdr; h.Get(headerToken) == "" {
		t.Fatalf("headers = %v", h)
	}
}

func TestUsageJSONPassesTheAnswerThrough(t *testing.T) {
	e, out, _, _ := peerEnv(t, nil, map[string]answer{"GET /api/usage": {200, usageJSON}})
	if code := run(e, "usage", "--json"); code != 0 || !strings.Contains(out.String(), `"top_handoffs"`) {
		t.Fatalf("exit %d %s", code, out)
	}
}
