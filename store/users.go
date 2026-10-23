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
}

// Validate checks that a binding names the scope its role needs.
func (b Binding) Validate() error {
	switch b.Role {
	case RoleProxyAdmin, RoleProxyViewer:
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
