package inbound

import (
	"time"

	"github.com/google/uuid"
)

type Request struct {
	// Which tenant received this. Everything recorded for it is visible only
	// inside that tenant.
	Tenant     uuid.UUID
	Provider   string
	Path       string
	ReceivedAt time.Time
	Headers    map[string][]string
	// How the signature stood when the request arrived. Empty means nothing
	// checked it.
	Signature string
	// Exactly the bytes received. Signature verification is computed over
	// them, so they are never re-encoded.
	Body []byte
}
