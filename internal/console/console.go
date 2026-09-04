package console

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID           uuid.UUID
	Email        string
	PasswordHash string
	Subject      string
}

// A user provisioned through single sign-on has no password, and must not be
// able to sign in with one.
func (u User) CanSignInWithPassword() bool { return u.PasswordHash != "" }

type Filter struct {
	Provider string
	State    string
	Search   string
	Since    time.Time
	Until    time.Time
	Page     int
	PageSize int
}

func (f Filter) Offset() int {
	if f.Page < 1 {
		return 0
	}
	return (f.Page - 1) * f.Size()
}

func (f Filter) Size() int {
	if f.PageSize <= 0 || f.PageSize > 200 {
		return 50
	}
	return f.PageSize
}

type EventSummary struct {
	ID         uuid.UUID
	Provider   string
	Path       string
	ReceivedAt time.Time
	BodySize   int32
	Planned    bool
	Deliveries int64
	Delivered  int64
	Dead       int64
	Pending    int64
	// Deliveries left over from a destination that is no longer routed. Kept
	// out of the counts above so they reconcile with the routes page.
	Unrouted int64
	// The highest attempt count among the routed deliveries, so the list shows
	// what is struggling without opening it.
	Attempts int32
}

type EventDetail struct {
	ID         uuid.UUID
	Provider   string
	Path       string
	ReceivedAt time.Time
	BodySize   int32
	Planned    bool
	Headers    map[string][]string
	Body       []byte
}

type Delivery struct {
	ID            uuid.UUID
	Destination   string
	URL           string
	State         string
	Attempts      int32
	NextAttemptAt time.Time
	LastStatus    int32
	LastError     string
	DeliveredAt   time.Time
	ReplayCount   int32
	ReplayedAt    time.Time
	// Whether the destination is still routed for this event's provider and
	// enabled. A delivery that is not stays as history and is never resent.
	Routed bool
	Rounds []Round
}

// A run of attempts. Round zero is the original delivery; every replay opens
// the next one, and each keeps its own attempt numbering.
type Round struct {
	Number   int32
	Attempts []Attempt
}

func (r Round) Title() string {
	if r.Number == 0 {
		return "attempts"
	}
	return fmt.Sprintf("after replay %d", r.Number)
}

type Attempt struct {
	Round       int32
	Attempt     int32
	AttemptedAt time.Time
	Status      int32
	Error       string
	DurationMs  int32
}

type RouteRow struct {
	ID            uuid.UUID
	Provider      string
	DestinationID uuid.UUID
	Destination   string
	URL           string
	Enabled       bool
	Deliveries    int64
}

// A provider whose events have been recorded but that has no route at all, so
// nothing it sent has anywhere to go.
type UnroutedProvider struct {
	Provider     string
	Events       int64
	LastReceived time.Time
}

var ErrWrongPassword = errors.New("wrong email or password")

func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hashing the password: %w", err)
	}
	return string(hash), nil
}

func CheckPassword(hash, password string) error {
	if hash == "" {
		return ErrWrongPassword
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return ErrWrongPassword
	}
	return nil
}

// The cookie carries the token; the database stores only its digest, so a
// leaked table does not hand over live sessions.
func NewSessionToken() (cookie string, digest []byte, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generating a session token: %w", err)
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), sum[:], nil
}

func SessionDigest(cookie string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cookie)
	if err != nil {
		return nil, fmt.Errorf("decoding the session token: %w", err)
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}
