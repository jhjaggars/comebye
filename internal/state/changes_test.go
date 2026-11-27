package state

import (
	"testing"
	"time"
)

func TestChangesDebounce(t *testing.T) {
	c := NewChanges()
	c.SetDebounce(100 * time.Millisecond)
	ch, cancel := c.Subscribe()
	defer cancel()
	for i := 0; i < 10; i++ {
		c.Changed(ChangedTodos)
	}
	c.Changed(ChangedPages)
	got := map[string]int{}
	timeout := time.After(400 * time.Millisecond)
loop:
	for {
		select {
		case ev := <-ch:
			got[ev.Kind]++
		case <-timeout:
			break loop
		}
	}
	// todos: one at once, one trailing for the rest of the burst; pages: one.
	if got[ChangedTodos] != 2 || got[ChangedPages] != 1 {
		t.Fatalf("got %v, want todos 2 and pages 1", got)
	}
	c.Changed("nonsense")
	c.Changed(ChangedJobPrefix)
	select {
	case ev := <-ch:
		t.Fatalf("an unknown kind went out: %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
}
