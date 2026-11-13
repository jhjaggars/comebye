package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"time"
)

// pendingSchedule reads a claude session file and says whether it still has a
// loop or a wakeup scheduled: a CronCreate (recurring) not followed by a
// CronDelete, or a ScheduleWakeup (without stop) whose time has not come. It is
// the one signal there is for this; the harness keeps no list the hub can read.
func pendingSchedule(path string, now time.Time) (pending bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var cron bool
	var wakeAt time.Time
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 && (bytes.Contains(line, []byte(`"CronCreate"`)) || bytes.Contains(line, []byte(`"CronDelete"`)) || bytes.Contains(line, []byte(`"ScheduleWakeup"`))) {
			applyScheduleLine(line, &cron, &wakeAt)
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return false, rerr
		}
	}
	return cron || wakeAt.After(now), nil
}

func applyScheduleLine(line []byte, cron *bool, wakeAt *time.Time) {
	var rec struct {
		Timestamp time.Time `json:"timestamp"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return
	}
	var blocks []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Input struct {
			Recurring    *bool   `json:"recurring"`
			Stop         bool    `json:"stop"`
			DelaySeconds float64 `json:"delaySeconds"`
		} `json:"input"`
	}
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		switch b.Name {
		case "CronCreate":
			if b.Input.Recurring == nil || *b.Input.Recurring {
				*cron = true
			}
		case "CronDelete":
			*cron = false
		case "ScheduleWakeup":
			if b.Input.Stop {
				*wakeAt = time.Time{}
			} else {
				*wakeAt = rec.Timestamp.Add(time.Duration(b.Input.DelaySeconds * float64(time.Second)))
			}
		}
	}
}
