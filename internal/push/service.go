package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Message is the JSON payload the service worker receives.
type Message struct {
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
	URL      string `json:"url,omitempty"` // same-origin path, e.g. /#/agent/<pane>
	Tag      string `json:"tag,omitempty"` // a newer notification with the same tag replaces the older
	Renotify bool   `json:"renotify,omitempty"`
}

// Options are per-message Web Push headers.
type Options struct {
	TTL     time.Duration // how long the push service keeps an undelivered message
	Urgency string        // very-low | low | normal | high
	Topic   string        // collapses undelivered messages with the same topic
}

// Config configures a Service.
type Config struct {
	// Dir is the state directory holding the VAPID key and subscriptions.
	Dir string
	// Subject is the VAPID sub claim (mailto: or https: contact).
	Subject string
	// ExcerptChars caps the last-message excerpt in notification bodies.
	ExcerptChars int
	// MinInterval throttles repeat notifications per key (agent+kind).
	MinInterval time.Duration
	// Workers bounds concurrent requests to push services (default 4).
	Workers int
	// QueueSize bounds pending sends; beyond it sends are dropped (default 128).
	QueueSize int

	// HTTPClient and CheckEndpoint override the hardened defaults
	// (public-address dialer, ValidateEndpoint). For tests only.
	HTTPClient    *http.Client
	CheckEndpoint func(string) error
}

// Service sends Web Push notifications to stored subscriptions.
type Service struct {
	cfg    Config
	key    *ecdsa.PrivateKey
	pubKey string // base64url uncompressed point
	store  *Store

	client   *http.Client
	validate func(string) error // endpoint check at send time (tests relax it)
	now      func() time.Time

	queue     chan job
	startOnce sync.Once

	mu   sync.Mutex
	last map[string]time.Time
}

type job struct {
	sub     Subscription
	payload []byte
	opts    Options
}

// New loads (or creates) the VAPID key and subscription store in cfg.Dir.
func New(cfg Config) (*Service, error) {
	if err := ValidateSubject(cfg.Subject); err != nil {
		return nil, err
	}
	key, err := LoadOrCreateVAPID(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("vapid key: %w", err)
	}
	store, err := OpenStore(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("push subscriptions: %w", err)
	}
	return newService(cfg, key, store)
}

func newService(cfg Config, key *ecdsa.PrivateKey, store *Store) (*Service, error) {
	pub, err := publicKeyBytes(key)
	if err != nil {
		return nil, err
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 128
	}
	svc := &Service{
		cfg:      cfg,
		key:      key,
		pubKey:   b64.EncodeToString(pub),
		store:    store,
		client:   newHTTPClient(),
		validate: ValidateEndpoint,
		now:      time.Now,
		queue:    make(chan job, cfg.QueueSize),
		last:     map[string]time.Time{},
	}
	if cfg.HTTPClient != nil {
		svc.client = cfg.HTTPClient
	}
	if cfg.CheckEndpoint != nil {
		svc.validate = cfg.CheckEndpoint
	}
	return svc, nil
}

// PublicKey is the VAPID public key for pushManager.subscribe.
func (s *Service) PublicKey() string { return s.pubKey }

// Store returns the subscription store.
func (s *Service) Store() *Store { return s.store }

// Active reports whether there is anyone to notify.
func (s *Service) Active() bool { return s != nil && s.store.Len() > 0 }

// ExcerptChars is the configured message excerpt length.
func (s *Service) ExcerptChars() int { return s.cfg.ExcerptChars }

// allow applies per-key throttling and records the send.
func (s *Service) allow(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if t, ok := s.last[key]; ok && now.Sub(t) < s.cfg.MinInterval {
		return false
	}
	if len(s.last) >= maxThrottleKeys {
		// Entries past the interval no longer throttle anything.
		for k, t := range s.last {
			if now.Sub(t) >= s.cfg.MinInterval {
				delete(s.last, k)
			}
		}
	}
	s.last[key] = now
	return true
}

// maxThrottleKeys is the throttle table size at which lapsed entries are
// pruned, so agents that come and go cannot grow it for good.
const maxThrottleKeys = 1024

// Notify queues msg for every subscription. It never blocks: sends run on
// a small worker pool, and if the queue is full the send is dropped and
// logged. key is the throttle bucket (agent and kind).
func (s *Service) Notify(key string, msg Message, opts Options) {
	if !s.Active() || !s.allow(key) {
		return
	}
	payload, err := encodePayload(msg)
	if err != nil {
		log.Printf("push: %v", err)
		return
	}
	s.startOnce.Do(s.startWorkers)
	for _, sub := range s.store.List() {
		select {
		case s.queue <- job{sub: sub, payload: payload, opts: opts}:
		default:
			log.Printf("push: queue full, dropping notification for %s", redactEndpoint(sub.Endpoint))
		}
	}
}

func (s *Service) startWorkers() {
	for range s.cfg.Workers {
		go func() {
			for j := range s.queue {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if _, err := s.Send(ctx, j.sub, j.payload, j.opts); err != nil {
					log.Printf("push: %v", err)
				}
				cancel()
			}
		}()
	}
}

// TopicFor derives a Topic header value (≤32 base64url chars) from a key
// such as a pane id, so undelivered pushes for the same agent collapse.
func TopicFor(key string) string {
	sum := sha256.Sum256([]byte(key))
	return b64.EncodeToString(sum[:24])
}

// encodePayload marshals msg, shortening the body if needed to fit one
// record.
func encodePayload(msg Message) ([]byte, error) {
	for {
		b, err := json.Marshal(msg)
		if err != nil {
			return nil, err
		}
		if len(b) <= MaxPlaintext {
			return b, nil
		}
		if msg.Body == "" {
			return nil, errors.New("push payload too large")
		}
		r := []rune(msg.Body)
		msg.Body = string(r[:len(r)*3/4]) + "…"
	}
}

// ErrGone means the push service no longer knows the subscription
// (404/410); it has been removed from the store.
var ErrGone = errors.New("subscription expired")

// Send encrypts and delivers one payload synchronously, returning the push
// service's HTTP status. A 404 or 410 removes the subscription and returns
// ErrGone.
func (s *Service) Send(ctx context.Context, sub Subscription, payload []byte, opts Options) (int, error) {
	if err := s.validate(sub.Endpoint); err != nil {
		return 0, fmt.Errorf("refusing endpoint %s: %w", redactEndpoint(sub.Endpoint), err)
	}
	body, err := Encrypt(payload, sub.Keys)
	if err != nil {
		return 0, fmt.Errorf("encrypt for %s: %w", redactEndpoint(sub.Endpoint), err)
	}
	aud, err := audience(sub.Endpoint)
	if err != nil {
		return 0, err
	}
	jwt, err := vapidJWT(s.key, aud, s.cfg.Subject, s.now())
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", strconv.Itoa(int(ttl/time.Second)))
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+s.pubKey)
	if opts.Urgency != "" {
		req.Header.Set("Urgency", opts.Urgency)
	}
	if opts.Topic != "" {
		req.Header.Set("Topic", opts.Topic)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send to %s: %w", redactEndpoint(sub.Endpoint), err)
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp.StatusCode, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		if _, err := s.store.Delete(sub.Endpoint); err != nil {
			log.Printf("push: removing expired subscription: %v", err)
		}
		log.Printf("push: removed expired subscription %s (HTTP %d)", redactEndpoint(sub.Endpoint), resp.StatusCode)
		return resp.StatusCode, ErrGone
	default:
		// Apple answers 403 {"reason":"BadJwtToken"} for a bad subject.
		return resp.StatusCode, fmt.Errorf("send to %s: HTTP %d %s", redactEndpoint(sub.Endpoint), resp.StatusCode, bytes.TrimSpace(detail))
	}
}

// redactEndpoint keeps the push service origin for logs; the path is a
// bearer capability for sending to that browser.
func redactEndpoint(endpoint string) string {
	if aud, err := audience(endpoint); err == nil {
		return aud + "/…"
	}
	return "(invalid endpoint)"
}
