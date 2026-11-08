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
	t.Helper()
	tp, fp, fn, tn := 0, 0, 0, 0
	for _, r := range rows {
		got := Classify(r.Text).Ask
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
