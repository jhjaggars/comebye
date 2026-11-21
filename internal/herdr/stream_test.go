package herdr

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// hangupHerdr acks each events.subscribe and hangs up, so a Stream on it
// resyncs right away, again and again. It records each subscription set's size.
type hangupHerdr struct {
	mu    sync.Mutex
	sizes []int
}

func (h *hangupHerdr) got() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.sizes...)
}

func startHangupHerdr(t *testing.T) (*hangupHerdr, *Client) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hs") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	h := &hangupHerdr{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req struct {
					ID     string `json:"id"`
					Params struct {
						Subscriptions []Subscription `json:"subscriptions"`
					} `json:"params"`
				}
				_ = json.Unmarshal(line, &req)
				h.mu.Lock()
				h.sizes = append(h.sizes, len(req.Params.Subscriptions))
				h.mu.Unlock()
				b, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"type": "subscription_started"}})
				c.Write(append(b, '\n'))
			}()
		}
	}()
	return h, NewClient(sock)
}

func waitSizes(t *testing.T, h *hangupHerdr, what string, ok func([]int) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok(h.got()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: subscribes = %v", what, h.got())
}

func subsOf(n int) []Subscription {
	subs := []Subscription{}
	for i := 0; i < n; i++ {
		subs = append(subs, StatusSubscription("p"+string(rune('a'+i))))
	}
	return subs
}

// onResync runs on the stream's own loop; restarting from there with a new
// set must reconnect, not wait for itself (risk R1).
func TestStreamRestartFromOnResync(t *testing.T) {
	h, client := startHangupHerdr(t)
	var mu sync.Mutex
	want := 1
	var s *Stream
	s = NewStream(client, nil, func(string) {
		mu.Lock()
		n := want
		mu.Unlock()
		s.Start(subsOf(n))
	})
	s.Start(subsOf(1))
	waitSizes(t, h, "first subscribe", func(g []int) bool { return len(g) > 0 })
	for _, n := range []int{2, 3, 1} {
		mu.Lock()
		want = n
		mu.Unlock()
		waitSizes(t, h, "restart", func(g []int) bool { return len(g) > 0 && g[len(g)-1] == n })
	}
	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung")
	}
}

// Stop while a loop is inside onResync, which calls Start: Stop returns once
// onResync does, and that Start does not bring the stream back.
func TestStreamStopDuringOnResync(t *testing.T) {
	h, client := startHangupHerdr(t)
	inResync := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	var s *Stream
	n := 1
	s = NewStream(client, nil, func(string) {
		once.Do(func() {
			inResync <- struct{}{}
			<-release
		})
		n++
		s.Start(subsOf(n))
	})
	s.Start(subsOf(1))
	<-inResync
	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	time.Sleep(20 * time.Millisecond) // let Stop mark the stream stopped
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung while onResync called Start")
	}
	before := len(h.got())
	time.Sleep(700 * time.Millisecond) // longer than the first backoff
	if after := len(h.got()); after != before {
		t.Fatalf("stream came back after Stop: %d subscribes, then %d", before, after)
	}
}
