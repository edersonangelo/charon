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
	// Signing names where the secrets are that this delivery is signed with,
	// never the secrets. They are read at the moment of the attempt, so a
	// rotation takes effect on the next one.
	Signing []string
}

type Route struct {
	Provider    string
	Destination string
	Transport   string
	URL         string
	Enabled     bool
	// Signed is how many secrets the destination signs with. Zero is a
	// destination whose receiver cannot tell a delivery from here apart from
	// anything else that reaches its address.
	Signed int
}
