package outbound

import "context"

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
	// Retryable is false when the transport knows another attempt cannot
	// help, so a delivery is not retried twelve times against a rejection
	// that will never change.
	Retryable bool
}

// Transport hands a delivery to a destination. One implementation per kind of
// destination; the delivery loop owns attempts, backoff and leases, and knows
// nothing about how the handover happens.
type Transport interface {
	Send(ctx context.Context, delivery Delivery) Result
}
