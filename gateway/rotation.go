package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/secret"
	"github.com/go-llmflow/gateway/store"
)

// Scheduled key rotation. A key with a schedule gets a new secret every
// `every`; the new secret is written to an OpenBao/Vault KV v2 field under
// key_rotation.vault_prefix/<team>/, where the client reads it, and the
// replaced secret stays valid for the grace period so the client has time
// to pick the new one up (LiteLLM: auto_rotate, rotation_interval,
// LITELLM_KEY_ROTATION_GRACE_PERIOD).
//
// Every replica runs the check. A rotation is conditional on the key's
// current hash, so only one replica rotates a key, and only that one
// writes the secret. If the write fails the rotation is undone (the key
// is due again at the next check) rather than leaving a secret nobody has.

// SecretWriter writes a secret to a vault reference (secret.Resolver).
type SecretWriter interface {
	Write(ctx context.Context, ref secret.Ref, value string) error
}

// WithSecretWriter sets where scheduled rotations deliver new secrets.
func WithSecretWriter(w SecretWriter) Option { return func(s *Server) { s.secretWriter = w } }

const (
	minRotationEvery = time.Hour
	maxRotationEvery = 366 * 24 * time.Hour
)

// validTarget checks that a delivery target is a vault field under the
// configured prefix and the key's own team, so that a team admin can only
// write where the team's secrets live.
func (s *Server) validTarget(target, team string) bool {
	kr := s.cfg.KeyRotation
	if kr == nil {
		return false
	}
	r, err := secret.Parse(target)
	if err != nil || !r.IsVault() || r.Version != 0 {
		return false
	}
	return strings.HasPrefix(r.Mount+"/"+r.Path, kr.VaultPrefix+"/"+team+"/")
}

// setKeyRotation sets or clears a key's schedule:
// {"every":"720h","grace_period":"24h","target":"vault:…#field"}; every
// "0s" (or absent) clears it.
func (s *Server) setKeyRotation(w http.ResponseWriter, r *http.Request, p *principal) {
	var in store.Rotation
	if !readJSON(w, r, &in) {
		return
	}
	k, err := s.st.GetKey(r.Context(), r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		return
	}
	t, ok := s.team(w, r, k.Team)
	if !ok {
		return
	}
	if !p.teamAdmin(t) && !(p.teamMember(t) && k.Owner != "" && k.Owner == p.name) {
		forbidden(w)
		return
	}
	if in.Every != 0 {
		switch {
		case s.cfg.KeyRotation == nil:
			apiError(w, http.StatusBadRequest, "invalid_request_error", "scheduled rotation is not configured (key_rotation block)")
			return
		case time.Duration(in.Every) < minRotationEvery || time.Duration(in.Every) > maxRotationEvery:
			apiError(w, http.StatusBadRequest, "invalid_request_error", "every: between 1h and 8784h")
			return
		case in.Grace <= 0 || time.Duration(in.Grace) > maxGracePeriod || in.Grace >= in.Every:
			// A scheduled rotation without overlap would cut the client off
			// until it reads the new secret.
			apiError(w, http.StatusBadRequest, "invalid_request_error", "grace_period: above 0, at most 720h and shorter than every")
			return
		case !s.validTarget(in.Target, k.Team):
			apiError(w, http.StatusBadRequest, "invalid_request_error", "target: want vault:"+s.cfg.KeyRotation.VaultPrefix+"/"+k.Team+"/<path>#<field>")
			return
		}
	} else {
		in = store.Rotation{}
	}
	if err := s.st.SetKeyRotation(r.Context(), k.ID, in); err != nil {
		storeError(w, err)
		return
	}
	s.audit("key.rotation_scheduled", "key", k.ID, "team", k.Team, "by", p.name,
		"every", time.Duration(in.Every).String(), "grace_period", time.Duration(in.Grace).String(), "target", in.Target)
	writeJSON(w, http.StatusOK, in)
}

// rotationLoop checks for due keys every check_interval.
func (s *Server) rotationLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.KeyRotation.CheckInterval)
	defer t.Stop()
	for {
		s.rotateDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// rotateDue rotates every due key; it returns the number rotated.
func (s *Server) rotateDue(ctx context.Context) int {
	if s.secretWriter == nil {
		return 0
	}
	now := s.now()
	due, err := s.st.DueKeys(ctx, now)
	if err != nil {
		s.log.Error("key rotation: listing due keys failed", "err", err)
		return 0
	}
	n := 0
	for _, k := range due {
		if ctx.Err() != nil {
			return n
		}
		if s.rotateScheduled(ctx, k, now) {
			n++
		}
	}
	return n
}

func (s *Server) rotateScheduled(ctx context.Context, k store.Key, now time.Time) bool {
	ref, err := secret.Parse(k.Rotation.Target)
	if err != nil || !s.validTarget(k.Rotation.Target, k.Team) {
		// The prefix may have changed since the schedule was set.
		s.audit("key.rotation_failed", "key", k.ID, "team", k.Team, "reason", "target outside key_rotation.vault_prefix/<team>/")
		return false
	}
	newSecret, newHash := store.NewKeySecret()
	err = s.st.RotateKey(ctx, k.ID, newHash, k.Hash, now.Add(time.Duration(k.Rotation.Grace)), now)
	switch {
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
		return false // another replica rotated it, or it was revoked
	case err != nil:
		s.log.Error("key rotation failed", "key", k.ID, "err", err)
		return false
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.secretWriter.Write(wctx, ref, newSecret); err != nil {
		// Nobody has the new secret: undo, so the key keeps working with
		// the old one and is due again at the next check.
		uerr := s.st.RotateKey(context.WithoutCancel(ctx), k.ID, k.Hash, newHash, time.Time{}, k.RotatedAt)
		s.audit("key.rotation_failed", "key", k.ID, "team", k.Team, "reason", "delivery: "+err.Error(), "undone", uerr == nil)
		if uerr != nil {
			s.log.Error("key rotation: undo failed; the key's new secret was not delivered", "key", k.ID, "err", uerr)
		}
		s.dropCached("key", k.ID, "")
		return false
	}
	s.dropCached("key", k.ID, "")
	s.audit("key.rotated", "key", k.ID, "team", k.Team, "by", "schedule", "grace_period", time.Duration(k.Rotation.Grace).String(), "target", k.Rotation.Target)
	return true
}
