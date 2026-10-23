// Package store holds the gateway's persistent state: teams, virtual keys
// (hashed), budgets and per-request usage records. Two implementations
// share one contract test: Memory (tests, trials; state is lost on
// restart) and Postgres (pgx).
package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Errors.
var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

// Team is a tenant.
type Team struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Disabled  bool      `json:"disabled"`
}

// Key is a virtual key. The secret itself is never stored, only its
// SHA-256.
type Key struct {
	ID          string    `json:"id"`
	Hash        [32]byte  `json:"-"`
	Team        string    `json:"team"`
	Name        string    `json:"name"`
	RPM         int       `json:"rpm"`
	TPM         int       `json:"tpm"`
	MaxInFlight int       `json:"max_inflight"`
	CreatedAt   time.Time `json:"created_at"`
	Revoked     bool      `json:"revoked"`
}

// Period is a budget period, aligned on UTC calendar boundaries.
type Period string

// Periods.
const (
	Day   Period = "day"
	Week  Period = "week"
	Month Period = "month"
)

// Valid reports whether p is a known period.
func (p Period) Valid() bool { return p == Day || p == Week || p == Month }

// Start returns the start of the period containing now (UTC; weeks start
// on Monday).
func (p Period) Start(now time.Time) time.Time {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch p {
	case Week:
		wd := (int(day.Weekday()) + 6) % 7 // Monday = 0
		return day.AddDate(0, 0, -wd)
	case Month:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return day
	}
}

// Budget caps a team's cost (GPU-hours) per period.
type Budget struct {
	Team    string  `json:"team"`
	Period  Period  `json:"period"`
	MaxCost float64 `json:"max_cost"`
}

// Record is one finished request.
type Record struct {
	Time             time.Time
	Team             string
	KeyID            string
	Model            string
	Pool             string
	Upstream         string
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	Cost             float64
	LatencyMS        float64
	Status           int
	Priority         int
	Borrowing        bool
	Stream           bool
	// Estimated is true when vLLM's usage report never arrived (client
	// gone before the final usage chunk or before a non-streaming answer)
	// and the tokens were estimated by the gateway.
	Estimated bool
}

// TeamUsage aggregates records of a team and pool over a period.
type TeamUsage struct {
	Team             string
	Pool             string
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	GPUHours         float64
	// EstimatedRequests counts records whose tokens were estimated.
	EstimatedRequests int64
}

// Store is the persistence contract.
type Store interface {
	Ping(ctx context.Context) error
	CreateTeam(ctx context.Context, name string) error
	ListTeams(ctx context.Context) ([]Team, error)
	// DisableTeam disables a team and revokes all its keys.
	DisableTeam(ctx context.Context, name string) error
	CreateKey(ctx context.Context, k Key) error
	ListKeys(ctx context.Context, team string) ([]Key, error)
	RevokeKey(ctx context.Context, id string) error
	// KeyByHash returns the key with that hash, revoked or not, or
	// ErrNotFound. The caller must still compare hashes in constant time.
	KeyByHash(ctx context.Context, hash [32]byte) (Key, error)
	SetBudget(ctx context.Context, b Budget) error
	DeleteBudget(ctx context.Context, team string) error
	Budgets(ctx context.Context) ([]Budget, error)
	// SpendSince returns the cost of each team's records at or after since.
	SpendSince(ctx context.Context, since time.Time) (map[string]float64, error)
	InsertRecords(ctx context.Context, recs []Record) error
	// Usage aggregates records in [from, to) per team and pool, sorted by
	// team then pool.
	Usage(ctx context.Context, from, to time.Time) ([]TeamUsage, error)
	Close()
}

// ValidateBudget checks a budget before it is stored.
func ValidateBudget(b Budget) error {
	if !b.Period.Valid() {
		return fmt.Errorf("period %q: want day, week or month", b.Period)
	}
	if !(b.MaxCost >= 0) || b.MaxCost > 1e15 {
		return fmt.Errorf("max_cost %v: want 0..1e15", b.MaxCost)
	}
	return nil
}
