package outbound

import (
	"context"
	"time"
)

// HTTP is the transport a destination uses unless it says otherwise. The name
// lives here, with the domain, so storage does not have to reach into the
// delivery loop for it.
const HTTP = "http"

// Result is what one attempt to hand a delivery over produced. It is not an
// HTTP response: a transport that has no status codes reports Accepted and a
// Detail, and the delivery loop does not care which kind it was talking to.
type Result struct {
	// Accepted is the only thing that closes a delivery.
	Accepted bool
	// Status is the transport's own code when it has one, zero when it does
	// not. It is recorded for an operator to read, never interpreted here.
	Status int
	// Detail is what to show when an attempt did not succeed.
	Detail string
	// Answer is what the destination said back, bounded by whatever read it.
	// A status code is usually the whole answer and cannot be relied on to be:
	// a rejection carries a reason, and losing it leaves a number nobody can
	// act on.
	Answer Answer
	// Signed names the secrets the delivery went out signed with, never the
	// secrets. Recorded on the attempt because after a rotation the set that
	// is configured no longer says which one went out when.
	Signed []string
	// RetryAfter is how long the destination asked to be left alone, when it
	// said so. Zero means it did not, and the usual backoff decides.
	RetryAfter time.Duration
	// Retryable is false when the transport knows another attempt cannot
	// help, so a delivery is not retried twelve times against a rejection
	// that will never change.
	Retryable bool
}

// Answer is what came back, kept for somebody reading later rather than for
// anything here to interpret.
type Answer struct {
	Body []byte
	// Type is what the destination called it, so a reader knows whether the
	// bytes are worth showing.
	Type string
	// Truncated says the body was longer than what was kept.
	Truncated bool
}

// Transport hands a delivery to a destination. One implementation per kind of
// destination; the delivery loop owns attempts, backoff and leases, and knows
// nothing about how the handover happens.
type Transport interface {
	Send(ctx context.Context, delivery Delivery) Result
}
