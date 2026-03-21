package notify

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

// NotifyPayload is the JSON body sent to webhook endpoints.
type NotifyPayload struct {
	Event      string         `json:"event"`
	StreamKey  string         `json:"stream_key"`
	Protocol   string         `json:"protocol,omitempty"`
	RemoteAddr string         `json:"remote_addr,omitempty"`
	Timestamp  int64          `json:"timestamp"`
	Extra      map[string]any `json:"extra,omitempty"`
}

// BuildPayload creates a NotifyPayload from an event name and context.
func BuildPayload(eventName string, ctx *core.EventContext) *NotifyPayload {
	return &NotifyPayload{
		Event:      eventName,
		StreamKey:  ctx.StreamKey,
		Protocol:   ctx.Protocol,
		RemoteAddr: ctx.RemoteAddr,
		Timestamp:  time.Now().Unix(),
		Extra:      ctx.Extra,
	}
}

// HTTPSender delivers webhook notifications to configured endpoints.
type HTTPSender struct {
	endpoints []config.NotifyEndpointConfig
	client    *http.Client
	queue     chan *NotifyPayload
	done      chan struct{}
}

// NewHTTPSender creates a new HTTP webhook sender.
func NewHTTPSender(endpoints []config.NotifyEndpointConfig) *HTTPSender {
	return &HTTPSender{
		endpoints: endpoints,
		client:    &http.Client{Timeout: 5 * time.Second},
		queue:     make(chan *NotifyPayload, 1024),
		done:      make(chan struct{}),
	}
}

// Start begins the worker goroutine that drains the queue.
func (s *HTTPSender) Start() {
	go s.worker()
}

// Stop signals the worker to exit and waits for drain.
func (s *HTTPSender) Stop() {
	close(s.done)
}

// Send enqueues a payload for delivery. Non-blocking; drops if queue is full.
func (s *HTTPSender) Send(p *NotifyPayload) {
	select {
	case s.queue <- p:
	default:
		log.Printf("[notify] queue full, dropping event %s for %s", p.Event, p.StreamKey)
	}
}

func (s *HTTPSender) worker() {
	for {
		select {
		case p := <-s.queue:
			s.deliver(p)
		case <-s.done:
			// Drain remaining
			for {
				select {
				case p := <-s.queue:
					s.deliver(p)
				default:
					return
				}
			}
		}
	}
}

func (s *HTTPSender) deliver(p *NotifyPayload) {
	body, err := json.Marshal(p)
	if err != nil {
		log.Printf("[notify] marshal error: %v", err)
		return
	}

	for _, ep := range s.endpoints {
		if !matchEvent(ep.Events, p.Event) {
			continue
		}

		timeout := ep.Timeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		client := &http.Client{Timeout: timeout}

		retries := ep.Retry
		if retries <= 0 {
			retries = 1
		}

		s.deliverToEndpoint(client, ep, body, retries)
	}
}

func (s *HTTPSender) deliverToEndpoint(client *http.Client, ep config.NotifyEndpointConfig, body []byte, retries int) {
	for attempt := 0; attempt < retries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			time.Sleep(backoff)
		}

		req, err := http.NewRequest(http.MethodPost, ep.URL, bytes.NewReader(body))
		if err != nil {
			log.Printf("[notify] request error: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")

		if ep.Secret != "" {
			sig := computeHMAC(body, ep.Secret)
			req.Header.Set("X-Signature", sig)
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[notify] POST %s attempt %d/%d failed: %v", ep.URL, attempt+1, retries, err)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return // success
		}
		log.Printf("[notify] POST %s attempt %d/%d got status %d", ep.URL, attempt+1, retries, resp.StatusCode)
	}
}

// matchEvent checks if the payload event matches the endpoint's event filter.
// Empty filter means all events match.
func matchEvent(filter []string, event string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, e := range filter {
		if e == event {
			return true
		}
	}
	return false
}

// computeHMAC returns the hex-encoded HMAC-SHA256 of data using the given secret.
func computeHMAC(data []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}
