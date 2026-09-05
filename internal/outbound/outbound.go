package outbound

import "github.com/google/uuid"

type Delivery struct {
	ID       uuid.UUID
	EventID  uuid.UUID
	Attempts int32
	Replays  int32
	// Which transport carries this delivery. The address in URL is written in
	// whatever form that transport reads.
	Transport string
	URL       string
	Provider  string
	Headers   map[string][]string
	Body      []byte
}

type Route struct {
	Provider    string
	Destination string
	Transport   string
	URL         string
	Enabled     bool
}
