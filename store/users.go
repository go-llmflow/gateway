package store

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"
)

// Role is an administrative role. Roles are bound to a user on a scope:
// the whole proxy, an organisation or a team. The set follows LiteLLM's
// access-control roles (proxy admin, proxy admin viewer, org admin, team
// admin, member) and Slurm's account coordinators, who manage the
// sub-accounts and users of the accounts they coordinate.
type Role string

// Roles, from the most to the least privileged on a given scope.
const (
	// RoleProxyAdmin administers everything (like the master key).
	RoleProxyAdmin Role = "proxy_admin"
	// RoleProxyViewer reads everything and changes nothing.
	RoleProxyViewer Role = "proxy_viewer"
	// RoleFinanceViewer reads the reports of every organisation (spend,
	// exports, fluids, lending, chargeback, decision reports) and nothing
	// else: no listing of keys, teams or users, no audit log, no write. It is
	// what a platform agreement's finance office needs (#97) and is granted by
	// a proxy admin only.
	RoleFinanceViewer Role = "finance_viewer"
	// RoleOrgAdmin administers one organisation: its teams, projects,
	// keys, budgets and the roles of its members.
	RoleOrgAdmin Role = "org_admin"
	// RoleTeamAdmin administers one team: its projects, keys and members.
	RoleTeamAdmin Role = "team_admin"
	// RoleMember uses one team: creates, lists and revokes its own keys.
	RoleMember Role = "member"
	// RoleViewer reads one organisation or team: listings and spend.
	RoleViewer Role = "viewer"
)

// Binding grants a role on a scope. Exactly the fields the role needs are
// set: none for proxy roles, Org for org_admin (and viewer of an org), Team
// for team_admin, member (and viewer of a team).
type Binding struct {
	User string `json:"user"`
	Role Role   `json:"role"`
	Org  string `json:"org,omitempty"`
	Team string `json:"team,omitempty"`
	// Provenance says who created the binding (#104). It is not part of the
	// binding's identity: Key() drops it.
	Provenance Provenance `json:"provenance,omitempty"`
}

// Provenance is how a binding came to exist.
type Provenance string

const (
	// ProvUnknown marks a binding stored before v0.18.2 (the migration's
	// default). It is treated as legitimate: nothing says it was not.
	ProvUnknown    Provenance = ""
	ProvSCIM       Provenance = "scim"
	ProvSSOClaim   Provenance = "sso_claim"
	ProvProxyAdmin Provenance = "proxy_admin"
	// ProvOrgAdmin is a binding granted by an administrator of an
	// organisation or a team (not a proxy admin): it never makes a person
	// provisioned into the organisation.
	ProvOrgAdmin Provenance = "org_admin"
)

// Valid reports whether p is a provenance a binding may carry.
func (p Provenance) Valid() bool {
	switch p {
	case ProvUnknown, ProvSCIM, ProvSSOClaim, ProvProxyAdmin, ProvOrgAdmin:
		return true
	}
	return false
}

// Provisioned reports whether a binding of this provenance puts a person
// into an organisation: the identity side (SCIM, an SSO claim), a proxy
// admin, or a binding older than the provenance record.
func (p Provenance) Provisioned() bool { return p != ProvOrgAdmin && p.Valid() }

// Key is the binding without its provenance: what identifies it.
func (b Binding) Key() Binding { b.Provenance = ProvUnknown; return b }

// Validate checks that a binding names the scope its role needs.
func (b Binding) Validate() error {
	if !b.Provenance.Valid() {
		return fmt.Errorf("unknown provenance %q", b.Provenance)
	}
	switch b.Role {
	case RoleProxyAdmin, RoleProxyViewer, RoleFinanceViewer:
		if b.Org != "" || b.Team != "" {
			return fmt.Errorf("role %s takes no scope", b.Role)
		}
	case RoleOrgAdmin:
		if b.Org == "" || b.Team != "" {
			return fmt.Errorf("role %s needs an org and no team", b.Role)
		}
	case RoleTeamAdmin, RoleMember:
		if b.Team == "" || b.Org != "" {
			return fmt.Errorf("role %s needs a team and no org", b.Role)
		}
	case RoleViewer:
		if (b.Org == "") == (b.Team == "") {
			return fmt.Errorf("role %s needs exactly one of org or team", b.Role)
		}
	default:
		return fmt.Errorf("unknown role %q", b.Role)
	}
	return nil
}

// User is a person who administers or uses the gateway.
type User struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Disabled  bool      `json:"disabled"`
}

// UserToken is an admin API credential of a user; only its hash is kept.
type UserToken struct {
	ID        string    `json:"id"`
	Hash      [32]byte  `json:"-"`
	User      string    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	Revoked   bool      `json:"revoked"`
}

// AdminTokenPrefix starts every user admin token.
const AdminTokenPrefix = "llma-"

// GenerateAdminToken returns a new admin token secret (256 random bits),
// its ID and its SHA-256.
func GenerateAdminToken() (secret, id string, hash [32]byte) {
	var b [32]byte
	_, _ = rand.Read(b[:])
	secret = AdminTokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	var idb [8]byte
	_, _ = rand.Read(idb[:])
	return secret, "t_" + hex.EncodeToString(idb[:]), HashSecret(secret)
}
