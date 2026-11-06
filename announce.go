package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/tsauth"
)

// announceToHub registers this node with the hub as a peer, retrying
// until the hub answers definitively.
func announceToHub(up *fleet.Upstream, ts *tsauth.LocalAPI, name, rawURL string) {
	if up == nil {
		return
	}
	backoff := 2 * time.Second
	for {
		u := rawURL
		if u == "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			node, _, err := ts.Self(ctx)
			cancel()
			if err != nil || node == "" {
				log.Printf("announce: waiting for tailscaled: %v", err)
				time.Sleep(backoff)
				backoff = min(backoff*2, 5*time.Minute)
				continue
			}
			u = "https://" + node
		}
		body, _ := json.Marshal(map[string]string{"name": name, "url": u})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		status, _, data, err := up.Do(ctx, fleet.UpstreamRequest{
			Method: http.MethodPost, Segments: []string{"api", "peers", "announce"}, Body: body, MaxBody: 4 << 10,
		})
		cancel()
		switch {
		case err == nil && status < 300:
			log.Printf("announce: registered with the hub %s as %s at %s", up.URL(), name, u)
			return
		case err == nil && (status == 400 || status == 409):
			log.Printf("announce: the hub refused (HTTP %d): %s", status, strings.TrimSpace(string(data)))
			return
		case err == nil:
			log.Printf("announce: HTTP %d: %s", status, strings.TrimSpace(string(data)))
		default:
			log.Printf("announce: %v", fmt.Errorf("%w", err))
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, 5*time.Minute)
	}
}
