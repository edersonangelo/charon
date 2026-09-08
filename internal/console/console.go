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

// Tenant is the boundary everything recorded belongs to.
type Tenant struct {
	ID        uuid.UUID
	Slug      string
	Name      string
	CreatedAt time.Time
}

type User struct {
	ID           uuid.UUID
	Email        string
	PasswordHash string
	Subject      string
	// SystemAdmin is not a role: a role is held inside a tenant, and the whole
	// point of this is not being confined to one.
	SystemAdmin bool
}

// Membership is somebody belonging to a tenant, as a role. A person can hold
// several, and sees one at a time.
type Membership struct {
	Tenant uuid.UUID
	Slug   string
	Name   string
	Role   string
	// Since is when they joined. The oldest is where they started, which is
	// where they are put when they have not chosen.
	Since time.Time
}

// Placement is a tenant and the role held in it. It is what a mapping says and
// what joining a tenant records.
type Placement struct {
	Tenant uuid.UUID
	Role   string
}

// A user provisioned through single sign-on has no password, and must not be
// able to sign in with one.
func (u User) CanSignInWithPassword() bool { return u.PasswordHash != "" }

// Operator is someone who can sign in to the panel of one tenant.
type Operator struct {
	ID          uuid.UUID
	Email       string
	Role        string
	Subject     string
	CreatedAt   time.Time
	SystemAdmin bool
}

// SignsInWithSingleSignOn reports whether the account is linked to an identity
// provider, which is the difference between an operator created here and one
// that arrived from outside.
func (o Operator) SignsInWithSingleSignOn() bool { return o.Subject != "" }

// Mapping is a claim an identity provider sends and where it lands.
type Mapping struct {
	Method string
	Claim  string
	Tenant string
	Role   string
}

type Filter struct {
	Provider  string
	State     string
	Signature string
	Search    string
	Since     time.Time
	Until     time.Time
	Page      int
	PageSize  int
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
	Signature  string
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
	// Forced says an operator overruled a failed signature to have this
	// delivered. The signature itself still says what it said.
	Forced bool
}

type EventDetail struct {
	ID         uuid.UUID
	Provider   string
	Path       string
	ReceivedAt time.Time
	BodySize   int32
	Signature  string
	Planned    bool
	Headers    map[string][]string
	Body       []byte
	// Overruled is the decision that had this delivered although it did not
	// verify, when there was one. The signature above still says what it said.
	Overruled *Overruled
}

// Overruled is why something that failed verification was delivered anyway,
// and who said so.
type Overruled struct {
	Reason    string
	DecidedBy string
	DecidedAt time.Time
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
	// SignedWith is what this attempt went out signed with, as it stood then.
	// After a rotation the destination's current secrets no longer answer it.
	SignedWith string
	// Forced says the event's signature had failed and somebody decided it
	// should go anyway.
	Forced bool
}

type RouteRow struct {
	ID            uuid.UUID
	Provider      string
	DestinationID uuid.UUID
	Destination   string
	Transport     string
	URL           string
	Enabled       bool
	Deliveries    int64
	Signing       []SigningSecret
}

// One secret a destination signs with: where it is kept, when it was added,
// and what the process that delivers last found when it tried to read it.
// Whether it is readable is never decided here, because the panel does not run
// where the signing happens.
type SigningSecret struct {
	Reference string
	Added     time.Time
	Checked   bool
	Readable  bool
	Detail    string
	CheckedAt time.Time
}

// A destination that is delivered to and signs with nothing, which once
// signing exists is a finding rather than a default.
type UnsignedDestination struct {
	Name   string
	Routes int64
}

// A provider whose events have been recorded but that has no route at all, so
// nothing it sent has anywhere to go.
type UnroutedProvider struct {
	Provider     string
	Events       int64
	LastReceived time.Time
}

// Verification is a provider's signature settings as the panel shows them.
// The secret never appears here: only the name of the variable that holds it.
type Verification struct {
	Provider      string
	Verifier      string
	Scheme        string
	Algorithm     string
	Encoding      string
	Header        string
	SecretEnv     string
	SecretPresent bool
	Refused       int64
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
