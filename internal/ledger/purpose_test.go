package ledger

import "testing"

func TestEffectivePurpose(t *testing.T) {
	for _, tc := range []struct{ branch, purpose, want string }{
		{"research/etcd", "", "research"},
		{"investigate/x", "", "research"},
		{"plan/stop-pileup", "", "plan"},
		{"rfc/x", "", "plan"},
		{"rfc-0042", "", "plan"},
		{"spec/x", "", "plan"},
		{"review/pr-9", "", "review"},
		{"audit/x", "", "review"},
		{"spike/x", "", "spike"},
		{"poc/x", "", "poc"},
		{"proto/x", "", "poc"},
		{"eval/x", "", "eval"},
		{"test/x", "", "test"},
		{"smoke/x", "", "test"},
		{"docs/x", "", ""}, // docs is work to land
		{"feat/x", "", ""},
		{"fix/x", "", ""},
		{"x", "", ""},
		{"poc", "", ""},            // a prefix needs its slash
		{"rfcx/y", "", ""},         // and is not a substring match
		{"feat/x", "plan", "plan"}, // a declared purpose wins over the branch
		{"poc/x", "review", "review"},
		{"feat/x", "smoke", "test"}, // the alias
	} {
		e := Entry{Branch: tc.branch, Purpose: tc.purpose}
		if got := EffectivePurpose(e); got != tc.want {
			t.Errorf("EffectivePurpose(%q, %q) = %q, want %q", tc.branch, tc.purpose, got, tc.want)
		}
	}
}

func TestInferPurpose(t *testing.T) {
	for _, tc := range []struct{ declared, branch, purpose, source string }{
		{"", "poc/x", "poc", PurposeFromBranch},
		{"", "feat/x", "", PurposeFromNone},
		{"research", "poc/x", "research", PurposeFromFlag},
		{"smoke", "feat/x", "test", PurposeFromFlag},
	} {
		p, src := InferPurpose(tc.declared, tc.branch)
		if p != tc.purpose || src != tc.source {
			t.Errorf("InferPurpose(%q, %q) = %q %q, want %q %q", tc.declared, tc.branch, p, src, tc.purpose, tc.source)
		}
	}
	for _, p := range []string{"research", "plan", "review", "spike", "poc", "eval", "test", "smoke"} {
		if !DisposablePurpose(p) {
			t.Errorf("%s is not disposable", p)
		}
	}
	for _, p := range []string{"", "docs", "feature", "build"} {
		if DisposablePurpose(p) {
			t.Errorf("%q is disposable", p)
		}
	}
}
