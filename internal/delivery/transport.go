package delivery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/edersonangelo/charon/internal/outbound"
)

// HTTP names the transport this project ships. Re-exported so a caller wiring
// the registry does not need two packages.
const HTTP = outbound.HTTP

// Build makes a transport from the settings a deployment gave it.
type Build func(Options) (outbound.Transport, error)

// Options are what any transport may need. A transport reads the fields it
// understands and ignores the rest, so registering one does not change this
// type for the others.
type Options struct {
	RequestTimeout time.Duration
	Client         *http.Client
}

// Transports maps a kind of destination to the code that talks to it.
// Supporting a broker is a call to Register, not an edit to the delivery loop.
type Transports struct {
	builders map[string]Build
}

func NewTransports() *Transports {
	return &Transports{builders: map[string]Build{}}
}

func (t *Transports) Register(kind string, build Build) {
	t.builders[kind] = build
}

func (t *Transports) Kinds() []string {
	kinds := make([]string, 0, len(t.builders))
	for kind := range t.builders {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

func (t *Transports) Knows(kind string) bool {
	_, known := t.builders[kind]
	return known
}

var ErrUnknownTransport = errors.New("no transport is registered for that kind of destination")

// Build the transport for a kind. An unknown kind yields one that refuses:
// an attempt is recorded against it and not retried, because retrying cannot
// register a transport.
func (t *Transports) Build(kind string, options Options) outbound.Transport {
	if kind == "" {
		kind = HTTP
	}

	build, known := t.builders[kind]
	if !known {
		return refusing{detail: fmt.Sprintf("%s: %q", ErrUnknownTransport, kind)}
	}

	transport, err := build(options)
	if err != nil {
		return refusing{detail: err.Error()}
	}
	return transport
}

// DefaultTransports is what this project ships with.
func DefaultTransports() *Transports {
	transports := NewTransports()
	transports.Register(HTTP, buildHTTP)
	return transports
}

type refusing struct{ detail string }

func (r refusing) Send(context.Context, outbound.Delivery) outbound.Result {
	return outbound.Result{Detail: r.detail, Retryable: false}
}

type httpTransport struct {
	client  *http.Client
	timeout time.Duration
}

func buildHTTP(options Options) (outbound.Transport, error) {
	timeout := options.RequestTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return httpTransport{client: client, timeout: timeout}, nil
}

func (t httpTransport) Send(ctx context.Context, delivery outbound.Delivery) outbound.Result {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, delivery.URL, bytes.NewReader(delivery.Body))
	if err != nil {
		// A destination address that cannot even be turned into a request is
		// not going to become valid on the next attempt.
		return outbound.Result{Detail: "building the request: " + err.Error(), Retryable: false}
	}

	if contentType := header(delivery.Headers, "Content-Type"); contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Charon-Delivery-Id", delivery.ID.String())
	req.Header.Set("X-Charon-Event-Id", delivery.EventID.String())
	req.Header.Set("X-Charon-Provider", delivery.Provider)
	req.Header.Set("X-Charon-Attempt", strconv.Itoa(int(delivery.Attempts)+1))
	req.Header.Set("X-Charon-Replay", strconv.Itoa(int(delivery.Replays)))

	resp, err := t.client.Do(req)
	if err != nil {
		return outbound.Result{
			Detail:    "reaching the destination: " + err.Error(),
			Retryable: true,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	return outbound.Result{
		Accepted:  resp.StatusCode >= 200 && resp.StatusCode < 300,
		Status:    resp.StatusCode,
		Detail:    fmt.Sprintf("destination answered %d", resp.StatusCode),
		Retryable: true,
	}
}

func header(headers map[string][]string, name string) string {
	for key, values := range headers {
		if http.CanonicalHeaderKey(key) == name && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
