package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolFetchArgvAndResult(t *testing.T) {
	tip := strings.Repeat("a", 40)
	bin, dir := fakeWorker(t, `echo "kv1 docs/x is aaaa" >&2
echo '{"name":"kv1","branch":"docs/x","tip":"`+tip+`","local":"created","pushed":"pushed"}'
`)
	tool := &Tool{Bin: bin, Config: "/c"}
	r, err := tool.Fetch(context.Background(), FetchReq{Name: "kv1", Branch: "docs/x", Repo: "/home/u/code/comebye", RemoteRepo: "comebye"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Tip != tip || r.Pushed != "pushed" || r.Local != "created" {
		t.Errorf("result %+v", r)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if got := strings.TrimSpace(string(argv)); got != "-config /c -no-prompt fetch kv1 docs/x --repo /home/u/code/comebye --remote-repo comebye --push --json" {
		t.Errorf("argv %q", got)
	}
}

func TestToolFetchRefusesBadValuesBeforeRunning(t *testing.T) {
	bin, dir := fakeWorker(t, "exit 0\n")
	tool := &Tool{Bin: bin, Config: "/c"}
	ok := FetchReq{Name: "kv1", Branch: "docs/x", Repo: "/r/comebye", RemoteRepo: "comebye"}
	for name, mut := range map[string]func(*FetchReq){
		"bad name":         func(q *FetchReq) { q.Name = "KV1;id" },
		"option branch":    func(q *FetchReq) { q.Branch = "--upload-pack=id" },
		"refspec branch":   func(q *FetchReq) { q.Branch = "a:b" },
		"default branch":   func(q *FetchReq) { q.Branch = "main" },
		"relative repo":    func(q *FetchReq) { q.Repo = "code/comebye" },
		"remote repo path": func(q *FetchReq) { q.RemoteRepo = "../etc" },
		"no branch":        func(q *FetchReq) { q.Branch = "" },
	} {
		q := ok
		mut(&q)
		if _, err := tool.Fetch(context.Background(), q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "argv")); len(b) != 0 {
		t.Errorf("the tool ran for a refused request:\n%s", b)
	}
}

func TestToolFetchFailureCarriesTheToolsLine(t *testing.T) {
	bin, _ := fakeWorker(t, "echo 'worker: origin docs/x (abc) is not an ancestor of the worker'\"'\"'s (def): pushing would overwrite it; --force replaces it' >&2\nexit 1\n")
	_, err := (&Tool{Bin: bin, Config: "/c"}).Fetch(context.Background(), FetchReq{Name: "kv1", Branch: "docs/x", Repo: "/r/comebye", RemoteRepo: "comebye"})
	if err == nil || !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("err = %v", err)
	}
	bin, _ = fakeWorker(t, "echo 'not json'\n")
	if _, err := (&Tool{Bin: bin, Config: "/c"}).Fetch(context.Background(), FetchReq{Name: "kv1", Branch: "docs/x", Repo: "/r/comebye", RemoteRepo: "comebye"}); err == nil {
		t.Fatal("an answer that is not the result was accepted")
	}
}
