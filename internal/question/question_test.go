package question

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

type sample struct {
	Ask  bool   `json:"ask"`
	Text string `json:"text"`
	// Items is how many questions the message leaves (default 1 for an
	// ask): scored for Extract only.
	Items int `json:"items,omitempty"`
}

func load(t *testing.T, path string) []sample {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []sample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		var s sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, s)
	}
	return out
}

// score runs the classifier over a labelled corpus and reports precision
// and recall of "asks the user".
func score(t *testing.T, name string, rows []sample) (prec, rec float64) {
	return scoreWith(t, name, rows, func(s string) bool { return Classify(s).Ask })
}

func scoreWith(t *testing.T, name string, rows []sample, ask func(string) bool) (prec, rec float64) {
	t.Helper()
	tp, fp, fn, tn := 0, 0, 0, 0
	for _, r := range rows {
		got := ask(r.Text)
		switch {
		case got && r.Ask:
			tp++
		case got && !r.Ask:
			fp++
			t.Logf("%s: false positive: %q", name, r.Text)
		case !got && r.Ask:
			fn++
			t.Logf("%s: missed: %q", name, r.Text)
		default:
			tn++
		}
	}
	if tp+fp > 0 {
		prec = float64(tp) / float64(tp+fp)
	}
	if tp+fn > 0 {
		rec = float64(tp) / float64(tp+fn)
	}
	t.Logf("%s: %d messages (tp %d, fp %d, fn %d, tn %d): precision %.2f, recall %.2f", name, len(rows), tp, fp, fn, tn, prec, rec)
	return prec, rec
}

// The committed corpus is hand-written to the shapes real final messages
// take. A corpus of real transcripts can be scored too, without being
// committed: COMEBYE_QUESTION_CORPUS=file.jsonl go test ./internal/question -v
func TestCorpus(t *testing.T) {
	p, r := score(t, "corpus", load(t, "testdata/corpus.jsonl"))
	if p < 0.95 || r < 0.9 {
		t.Errorf("precision %.2f recall %.2f: want at least 0.95 and 0.90", p, r)
	}
}

// Extract must keep Classify's precision and recall on the same corpus and
// find the right number of questions in each message. "before" is Classify
// (the last paragraph only), "after" is Extract.
func TestExtractCorpus(t *testing.T) {
	rows := load(t, "testdata/corpus.jsonl")
	bp, br := scoreWith(t, "before (Classify)", rows, func(s string) bool { return Classify(s).Ask })
	ap, ar := scoreWith(t, "after  (Extract)", rows, func(s string) bool { return len(Extract(s).Items) > 0 })
	right, asks := 0, 0
	for _, r := range rows {
		if !r.Ask {
			continue
		}
		asks++
		want := max(r.Items, 1)
		if got := len(Extract(r.Text).Items); got == want {
			right++
		} else {
			t.Logf("item count: want %d, got %d: %q", want, got, r.Text)
		}
	}
	t.Logf("items: %d of %d asking messages have the right number of questions (Classify always yields at most 1)", right, asks)
	if ap < 0.95 || ar < 0.9 || ap < bp || ar < br {
		t.Errorf("extract precision %.2f recall %.2f (before %.2f / %.2f)", ap, ar, bp, br)
	}
	if right != asks {
		t.Errorf("%d of %d asking messages got the wrong question count", asks-right, asks)
	}
}

func TestLocalCorpus(t *testing.T) {
	path := os.Getenv("COMEBYE_QUESTION_CORPUS")
	if path == "" {
		t.Skip("set COMEBYE_QUESTION_CORPUS to score a local labelled corpus")
	}
	p, r := score(t, "local", load(t, path))
	if p < 0.9 || r < 0.8 {
		t.Errorf("precision %.2f recall %.2f", p, r)
	}
}

func TestResultFields(t *testing.T) {
	r := Classify("I tried both.\n\nShould I keep the old endpoint for one more release?\n\n- yes\n- no")
	if !r.Ask || r.Question != "Should I keep the old endpoint for one more release?" {
		t.Fatalf("%+v", r)
	}
	if want := "Should I keep the old endpoint for one more release?\n\n- yes\n- no"; r.Block != want {
		t.Fatalf("block = %q", r.Block)
	}
	if Classify("").Ask || Classify("   \n").Ask {
		t.Fatal("empty asks nothing")
	}
}
