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
	// PrevHash is the secret replaced by the last rotation, valid until
	// PrevExpires (zero: no previous secret).
	PrevHash    [32]byte  `json:"-"`
	PrevExpires time.Time `json:"prev_expires_at,omitzero"`
	RotatedAt   time.Time `json:"rotated_at,omitzero"`
	// Rotation is the key's rotation schedule, if any.
	Rotation Rotation `json:"rotation,omitzero"`
	// Policy names the request policy of the key (empty: its team's).
	Policy string `json:"policy,omitempty"`
	// Guardrails names guardrails the key adds to those of its org and
	// team (it can add, never remove).
	Guardrails []string `json:"guardrails,omitempty"`
}

// Matches reports whether h is the key's secret, or its previous secret
// still inside the rotation grace period at now.
func (k Key) Matches(h [32]byte, now time.Time) bool {
	if HashEqual(k.Hash, h) {
		return true
	}
	return !k.PrevExpires.IsZero() && now.Before(k.PrevExpires) && HashEqual(k.PrevHash, h)
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

// End returns the end of the period that starts at start (the next one's
// start): when a budget of this period resets.
func (p Period) End(start time.Time) time.Time {
	switch p {
	case Week:
		return start.AddDate(0, 0, 7)
	case Month:
		return start.AddDate(0, 1, 0)
	default:
		return start.AddDate(0, 0, 1)
	}
}

// Record is one finished request.
type Record struct {
	// ID identifies the record: inserting a record whose ID is already
	// stored does nothing, so the accounting spool can replay a batch that
	// may have been written before a crash without billing it twice. Empty
	// for records written before v0.10.0.
	ID      string
	Time    time.Time
	Org     string
	Team    string
	Project string
	KeyID   string
	// User is the gateway user the request is attributed to: the key's
	// owner, or a JWT's username; "" for a service key. Never a token.
	User string
	// Lab is the team's lab at the time of the request ("" for none).
	Lab              string
	Model            string
	Pool             string
	Upstream         string
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	// Cost is in Unit: "" for GPU-hours (vLLM models), a currency code
	// such as "USD" for an external provider. Costs of different units are
	// never added together (budgets and reports split by unit).
	Cost      float64
	Unit      string
	LatencyMS float64
	Status    int
	Priority  int
	Borrowing bool
	Stream    bool
	// Tags are the key's tags and the allow-listed request tags.
	Tags []string
	// Estimated is true when vLLM's usage report never arrived (client
	// gone before the final usage chunk or before a non-streaming answer)
	// and the tokens were estimated by the gateway.
	Estimated bool

	// Hard budgets (#87). CostNano is the cost in nano-units of Unit, as
	// the budget ledger counts it. A request admitted under budgets carries
	// its holds: the budget periods it was held against (Holds), by which
	// holder (HoldReplica, HoldIncarnation), for HoldNano, its worst-case
	// cost. Storing the record settles them in the same transaction:
	// spent += CostNano, the holder's grant -= min(CostNano, HoldNano).
	CostNano        int64
	AdmittedAt      time.Time
	HoldReplica     string
	HoldIncarnation string
	HoldNano        int64
	Holds           []HoldRef
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
	// RotateKey gives a key a new secret hash. The current one becomes
	// the previous secret, valid until prevExpires (a zero or past time
	// drops it at once), and RotatedAt becomes at (zero: none). With a
	// non-zero expect, the key's current hash must be expect (ErrConflict
	// otherwise), so that two replicas cannot rotate one key twice.
	// ErrNotFound for an unknown or revoked key, ErrExists when the new
	// hash is already another key's, or this key's current one.
	RotateKey(ctx context.Context, id string, newHash, expect [32]byte, prevExpires, at time.Time) error
	// SCIM provisioning documents (see SCIMDoc). SCIMCreate: ErrExists
	// when the id, or a User's name without case, is taken. SCIMReplace:
	// ErrConflict when the stored version is not ifVersion, ErrExists for a
	// taken name. SCIMList filters by name without case when name != "".
	SCIMCreate(ctx context.Context, d SCIMDoc) error
	SCIMGet(ctx context.Context, kind, id string) (SCIMDoc, error)
	SCIMReplace(ctx context.Context, d SCIMDoc, ifVersion string) error
	SCIMDelete(ctx context.Context, kind, id string) error
	SCIMList(ctx context.Context, kind, name string) ([]SCIMDoc, error)

	// The audit log (see AuditEntry). AuditAppend assigns Seq and PrevHash
	// under a lock held across replicas, calls hash, and stores the entry.
	// AuditPrune deletes the entries through seq (the caller checks that a
	// checkpoint anchors what remains).
	AuditAppend(ctx context.Context, e AuditEntry, hash AuditHasher) (AuditEntry, error)
	AuditRange(ctx context.Context, from int64, limit int) ([]AuditEntry, error)
	AuditAddCheckpoint(ctx context.Context, c AuditCheckpoint) error
	AuditCheckpoints(ctx context.Context) ([]AuditCheckpoint, error)
	AuditPrune(ctx context.Context, through int64) (int64, error)

	// SetKeyRotation sets a key's rotation schedule (every 0: none).
	SetKeyRotation(ctx context.Context, id string, r Rotation) error
	// DueKeys lists the keys, not revoked, whose scheduled rotation is due
	// at now: RotatedAt (or CreatedAt) + Every <= now.
	DueKeys(ctx context.Context, now time.Time) ([]Key, error)
	// KeyByHash returns the key whose current or previous hash is hash,
	// revoked or not, with its Org filled in, or ErrNotFound. The caller
	// must still check it with Key.Matches.
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

	// SetTeamTools replaces the MCP tool patterns a team may use
	// ("server.tool", "server.*" or "*"); an empty list allows none.
	SetTeamTools(ctx context.Context, team string, tools []string) error
	// TeamTools returns every team's non-empty tool list.
	TeamTools(ctx context.Context) (map[string][]string, error)

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
	BudgetLedger
	Close()
}
