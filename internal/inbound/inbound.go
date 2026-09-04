package inbound

import (
	"time"

	"github.com/google/uuid"
)

type Request struct {
	ID         uuid.UUID
	Provider   string
	Path       string
	ReceivedAt time.Time
	Headers    map[string][]string
	// Exactly the bytes received. Signature verification is computed over
	// them, so they are never re-encoded.
	Body []byte
}
