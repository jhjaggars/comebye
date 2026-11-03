package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/server"
	"github.com/jhjaggars/comebye/internal/tsauth"
)

// servePeerListener binds the peer listener to this node's tailnet
// address (spec: a port, or tailnet-ip:port) and serves it. tailscaled may
// not be up yet when the hub starts, so it retries instead of exiting.
func servePeerListener(srv *server.Server, ts *tsauth.LocalAPI, spec string, tags []string, selfReg bool) {
	spec = strings.TrimSpace(spec)
	host, port := "", strings.TrimPrefix(spec, ":")
	if h, p, err := net.SplitHostPort(spec); err == nil && h != "" {
		host, port = h, p
	}
	backoff := time.Second
	for {
		addr := host
		if addr == "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, ips, err := ts.Self(ctx)
			cancel()
			for _, ip := range ips {
				if ip.Is4() {
					addr = ip.String()
					break
				}
			}
			if err != nil || addr == "" {
				log.Printf("peer listener: waiting for tailscaled (%s): %v", ts.Socket, err)
				time.Sleep(backoff)
				if backoff *= 2; backoff > time.Minute {
					backoff = time.Minute
				}
				continue
			}
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(addr, port))
		if err != nil {
			log.Printf("peer listener: %v", err)
			time.Sleep(backoff)
			if backoff *= 2; backoff > time.Minute {
				backoff = time.Minute
			}
			continue
		}
		log.Printf("peer listener on http://%s for tags %s (agent role only; self-registration %v)", ln.Addr(), strings.Join(tags, ","), selfReg)
		log.Printf("peer listener stopped: %v", srv.ServePeerListener(ln))
		return
	}
}

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
