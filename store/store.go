// Package store holds the gateway's persistent state: the tenant
// hierarchy (organisation → team → project), virtual keys (hashed),
// budgets and per-request usage records. Two implementations share one
// contract test: Memory (tests, trials; state is lost on restart) and
// Postgres (pgx).
//
// The hierarchy mirrors the Slurm account tree (sacctmgr: accounts nested
// under a parent account, users associated with accounts): an organisation
// is an institution, a team a lab, a project a piece of work inside a team.
// Keys belong to a team and optionally to one of its projects.
package store

import (
	"context"
	"errors"
	"time"
)

// Errors.
var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	// ErrConflict reports an existing object that differs from the
	// request (e.g. a team that already belongs to another organisation).
	ErrConflict = errors.New("conflicts with an existing object")
)

// Org is an organisation (an institution), the top of the hierarchy.
type Org struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Disabled  bool      `json:"disabled"`
}

// Team is a group (a lab) inside an organisation.
type Team struct {
	Name      string    `json:"name"`
	Org       string    `json:"org"`
	CreatedAt time.Time `json:"created_at"`
	Disabled  bool      `json:"disabled"`
}

// Project is a unit of work inside a team, with its own keys and budget.
type Project struct {
	Team      string    `json:"team"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Disabled  bool      `json:"disabled"`
}

// Key is a virtual key. The secret itself is never stored, only its
// SHA-256.
type Key struct {
	ID   string   `json:"id"`
	Hash [32]byte `json:"-"`
	// Org is resolved from the team when the key is read.
	Org     string `json:"org"`
	Team    string `json:"team"`
	Project string `json:"project,omitempty"`
	// Owner is the user who created the key as a team member; empty for
	// team keys created by an administrator.
	Owner string `json:"owner,omitempty"`
	// Tags label every request made with the key (budgets and reports).
	Tags        []string  `json:"tags,omitempty"`
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

// Record is one finished request.
type Record struct {
	Time             time.Time
	Org              string
	Team             string
	Project          string
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
	// Tags are the key's tags and the allow-listed request tags.
	Tags []string
	// Estimated is true when vLLM's usage report never arrived (client
	// gone before the final usage chunk or before a non-streaming answer)
	// and the tokens were estimated by the gateway.
	Estimated bool
}

// Store is the persistence contract.
type Store interface {
	Ping(ctx context.Context) error

	CreateOrg(ctx context.Context, name string) error
	ListOrgs(ctx context.Context) ([]Org, error)
	// DisableOrg disables an organisation, its teams and projects, and
	// revokes all their keys.
	DisableOrg(ctx context.Context, name string) error

	// CreateTeam creates a team in an enabled organisation, re-enables a
	// disabled team of the same organisation, adopts a team created
	// without organisation (v0.1 databases), and returns ErrExists for an
	// enabled team of the same organisation, ErrConflict for a team of
	// another organisation, ErrNotFound for an unknown or disabled org.
	CreateTeam(ctx context.Context, name, org string) error
	GetTeam(ctx context.Context, name string) (Team, error)
	// ListTeams lists the teams of an organisation, or all with org "".
	ListTeams(ctx context.Context, org string) ([]Team, error)
	// DisableTeam disables a team and its projects and revokes its keys.
	DisableTeam(ctx context.Context, name string) error

	CreateProject(ctx context.Context, team, name string) error
	ListProjects(ctx context.Context, team string) ([]Project, error)
	// DisableProject disables a project and revokes its keys.
	DisableProject(ctx context.Context, team, name string) error

	// CreateKey stores a key of an enabled team (and project, when set).
	CreateKey(ctx context.Context, k Key) error
	// ListKeys lists keys, filtered by team when team is not "".
	ListKeys(ctx context.Context, team string) ([]Key, error)
	GetKey(ctx context.Context, id string) (Key, error)
	RevokeKey(ctx context.Context, id string) error
	// KeyByHash returns the key with that hash, revoked or not, with its
	// Org filled in, or ErrNotFound. The caller must still compare hashes
	// in constant time.
	KeyByHash(ctx context.Context, hash [32]byte) (Key, error)

	CreateUser(ctx context.Context, name string) error
	GetUser(ctx context.Context, name string) (User, error)
	ListUsers(ctx context.Context) ([]User, error)
	// DisableUser disables a user and revokes their admin tokens.
	DisableUser(ctx context.Context, name string) error
	CreateUserToken(ctx context.Context, t UserToken) error
	ListUserTokens(ctx context.Context, user string) ([]UserToken, error)
	RevokeUserToken(ctx context.Context, user, id string) error
	// UserByTokenHash returns the token with that hash and its user, or
	// ErrNotFound. The caller compares hashes in constant time and checks
	// Revoked and Disabled.
	UserByTokenHash(ctx context.Context, hash [32]byte) (User, UserToken, error)
	// Grant stores a binding; ErrNotFound when the user, org or team does
	// not exist, ErrExists when it is already granted.
	Grant(ctx context.Context, b Binding) error
	Ungrant(ctx context.Context, b Binding) error
	// Bindings lists the bindings of a user, or all with user "".
	Bindings(ctx context.Context, user string) ([]Binding, error)

	// SetTeamModels replaces the list of model groups a team may use; an
	// empty list allows every configured model.
	SetTeamModels(ctx context.Context, team string, models []string) error
	// TeamModels returns every team's non-empty model list.
	TeamModels(ctx context.Context) (map[string][]string, error)

	// SetBudget creates or replaces a budget's period, maximum and soft
	// limit; a temporary increase already granted is kept.
	SetBudget(ctx context.Context, b Budget) error
	// IncreaseBudget grants a temporary increase until expiry, replacing
	// any earlier one; ErrNotFound when the budget does not exist.
	IncreaseBudget(ctx context.Context, k BudgetKey, amount float64, expiry time.Time) error
	DeleteBudget(ctx context.Context, k BudgetKey) error
	Budgets(ctx context.Context) ([]Budget, error)
	// MarkSoftAlert records that the soft-limit alert of a budget was sent
	// for the period starting at period; it returns false when it already
	// was, so that several gateways or restarts send it once.
	MarkSoftAlert(ctx context.Context, k BudgetKey, period time.Time) (bool, error)
	// SpendRows aggregates the cost of records at or after since by org,
	// team, project, key, model and tag set.
	SpendRows(ctx context.Context, since time.Time) ([]SpendRow, error)
	InsertRecords(ctx context.Context, recs []Record) error
	// SetAllowlist replaces the client networks allowed for an
	// organisation, a team or a key; an empty list removes the allowlist.
	SetAllowlist(ctx context.Context, a Allowlist) error
	Allowlists(ctx context.Context) ([]Allowlist, error)
	// Report aggregates the records in [from, to) by the given dimensions
	// (see ReportDims), sorted.
	Report(ctx context.Context, from, to time.Time, dims []string) ([]ReportRow, error)
	Close()
}
