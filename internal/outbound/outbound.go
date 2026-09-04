package outbound

import "github.com/google/uuid"

type Delivery struct {
	ID       uuid.UUID
	EventID  uuid.UUID
	Attempts int32
	Replays  int32
	URL      string
	Provider string
	Headers  map[string][]string
	Body     []byte
}

type Route struct {
	Provider    string
	Destination string
	URL         string
	Enabled     bool
}
