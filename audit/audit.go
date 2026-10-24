// Package audit makes the gateway's audit log tamper-evident: every entry
// commits to the one before it, and checkpoints sign the chain.
//
// # Why a hash chain and not a Merkle tree
//
// RFC 6962 §2.1 (and RFC 9162, which obsoletes it) builds a Merkle tree so
// that a log can prove to a THIRD party, in O(log n), that one entry is in it
// (an audit path) and that a later tree extends an earlier one (a
// consistency proof). Here the verifier is the operator or an auditor who
// reads the whole log anyway: a hash chain gives the same guarantee for a
// full read — no entry can be altered, inserted, reordered or removed
// between two checkpoints without breaking every hash after it — with one
// hash per entry and a verifier of a few dozen lines. It also prunes by
// cutting at a checkpoint, which a tree only does with extra bookkeeping.
// What is borrowed from RFC 6962 is its domain separation: an entry hash is
// SHA-256(0x00 || previous hash || entry), and a checkpoint signs a message
// that names itself, so neither can be passed off as the other.
//
// What a chain cannot show is entries removed AFTER the last checkpoint,
// with nothing signed after them: the checkpoint interval bounds that window,
// and exporting the log to write-once storage closes it.
package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Store is what the audit log needs from the gateway's store.
type Store interface {
	AuditAppend(ctx context.Context, e store.AuditEntry, hash store.AuditHasher) (store.AuditEntry, error)
	AuditRange(ctx context.Context, from int64, limit int) ([]store.AuditEntry, error)
	AuditAddCheckpoint(ctx context.Context, c store.AuditCheckpoint) error
	AuditCheckpoints(ctx context.Context) ([]store.AuditCheckpoint, error)
	AuditPrune(ctx context.Context, through int64) (int64, error)
}

// canonical is the hashed form of an entry: a fixed field order, the time
// in microseconds, JSON strings as encoding/json writes them.
type canonical struct {
	V         int    `json:"v"`
	Seq       int64  `json:"seq"`
	Time      int64  `json:"time_us"`
	Kind      string `json:"kind"`
	Actor     string `json:"actor"`
	Roles     string `json:"roles"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Before    string `json:"before"`
	After     string `json:"after"`
	Status    int    `json:"status"`
	SourceIP  string `json:"source_ip"`
	RequestID string `json:"request_id"`
}

// CurrentVersion is the canonical form new entries are hashed with.
const CurrentVersion = 2

// Hash is an entry's hash: SHA-256(0x00 || PrevHash || canonical form).
//
// Version 1 (the first entries ever written) is the JSON of canonical.
// ⛔ encoding/json replaces invalid UTF-8 with U+FFFD, so two fields that
// differ only in invalid bytes hashed alike (review of #29). Version 2
// hashes the raw bytes of every field, each preceded by its length as a
// big-endian uint64, so no two different entries share an encoding.
// Version 1 is still verified for the rows that have it.
func Hash(e *store.AuditEntry) []byte {
	h := sha256.New()
	h.Write([]byte{0x00})
	h.Write(e.PrevHash)
	if e.V <= 1 {
		b, _ := json.Marshal(canonical{1, e.Seq, e.Time.UnixMicro(), e.Kind, e.Actor, e.Roles, e.Action, e.Target,
			e.Before, e.After, e.Status, e.SourceIP, e.RequestID})
		h.Write(b)
		return h.Sum(nil)
	}
	var n [8]byte
	field := func(b []byte) {
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	num := func(v int64) {
		binary.BigEndian.PutUint64(n[:], uint64(v))
		field(n[:])
	}
	field([]byte("llmflow-audit-entry/v2"))
	num(e.Seq)
	num(e.Time.UnixMicro())
	for _, s := range []string{e.Kind, e.Actor, e.Roles, e.Action, e.Target, e.Before, e.After} {
		field([]byte(s))
	}
	num(int64(e.Status))
	field([]byte(e.SourceIP))
	field([]byte(e.RequestID))
	return h.Sum(nil)
}

// CheckpointMessage is what a checkpoint signs.
func CheckpointMessage(seq int64, hash []byte, t time.Time) []byte {
	return []byte("llmflow-audit-checkpoint/v1\n" + strconv.FormatInt(seq, 10) + "\n" +
		hex.EncodeToString(hash) + "\n" + strconv.FormatInt(t.UTC().Truncate(time.Microsecond).UnixMicro(), 10) + "\n")
}

// KeyID names a public key: the first 8 bytes of its SHA-256, in hex.
func KeyID(pub ed25519.PublicKey) string {
	s := sha256.Sum256(pub)
	return hex.EncodeToString(s[:8])
}

// ParsePrivateKey reads a signing key: an Ed25519 seed (32 bytes) or
// private key (64 bytes), base64 or hex.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := decodeKey(s)
	if err != nil {
		return nil, errors.New("audit: the signing key is neither base64 nor hex")
	}
	switch len(b) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case ed25519.PrivateKeySize:
		k := ed25519.PrivateKey(b)
		if !k.Public().(ed25519.PublicKey).Equal(ed25519.NewKeyFromSeed(b[:32]).Public()) {
			return nil, errors.New("audit: the private key's public half does not match its seed")
		}
		return k, nil
	}
	return nil, fmt.Errorf("audit: a signing key of %d bytes (want a 32-byte Ed25519 seed)", len(b))
}

// decodeKey reads hex when the string is hex of a key size (hex digits are
// also base64, so hex is tried first), else standard base64.
func decodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if n := len(s); n == 2*ed25519.SeedSize || n == 2*ed25519.PrivateKeySize {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return base64.StdEncoding.DecodeString(s)
}

// ParsePublicKey reads a 32-byte Ed25519 public key, base64 or hex.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := decodeKey(s)
	if err != nil {
		return nil, errors.New("audit: the public key is neither base64 nor hex")
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("audit: a public key of %d bytes", len(b))
	}
	return ed25519.PublicKey(b), nil
}

// Log appends entries and signs checkpoints.
type Log struct {
	st  Store
	key ed25519.PrivateKey // nil: chain only, no checkpoints
	now func() time.Time
}

// New returns a log over st; key may be nil.
func New(st Store, key ed25519.PrivateKey, now func() time.Time) *Log {
	if now == nil {
		now = time.Now
	}
	return &Log{st: st, key: key, now: now}
}

// Append records e (Seq, PrevHash and Hash are assigned).
func (l *Log) Append(ctx context.Context, e store.AuditEntry) (store.AuditEntry, error) {
	if e.Time.IsZero() {
		e.Time = l.now()
	}
	e.V = CurrentVersion
	return l.st.AuditAppend(ctx, e, Hash)
}

// last returns the newest entry, or ok=false for an empty log.
func (l *Log) last(ctx context.Context) (store.AuditEntry, bool, error) {
	cps, err := l.st.AuditCheckpoints(ctx)
	if err != nil {
		return store.AuditEntry{}, false, err
	}
	from := int64(1)
	if n := len(cps); n > 0 {
		from = cps[n-1].Seq
	}
	var last store.AuditEntry
	found := false
	for {
		es, err := l.st.AuditRange(ctx, from, 1000)
		if err != nil {
			return last, false, err
		}
		if len(es) == 0 {
			return last, found, nil
		}
		last, found = es[len(es)-1], true
		from = last.Seq + 1
	}
}

// Checkpoint signs the chain up to its newest entry, unless the newest
// checkpoint already covers it. It reports the seq signed (0: nothing new).
func (l *Log) Checkpoint(ctx context.Context) (int64, error) {
	if l.key == nil {
		return 0, errors.New("audit: no signing key")
	}
	e, ok, err := l.last(ctx)
	if err != nil || !ok {
		return 0, err
	}
	cps, err := l.st.AuditCheckpoints(ctx)
	if err != nil {
		return 0, err
	}
	if n := len(cps); n > 0 && cps[n-1].Seq >= e.Seq {
		return 0, nil
	}
	t := l.now().UTC().Truncate(time.Microsecond)
	cp := store.AuditCheckpoint{Seq: e.Seq, Hash: e.Hash, Time: t, KeyID: KeyID(l.key.Public().(ed25519.PublicKey)),
		Signature: ed25519.Sign(l.key, CheckpointMessage(e.Seq, e.Hash, t))}
	if err := l.st.AuditAddCheckpoint(ctx, cp); err != nil && !errors.Is(err, store.ErrExists) {
		return 0, err
	}
	return e.Seq, nil
}

// Report is what Verify found.
type Report struct {
	First, Last int64 // entries present (0, 0: none)
	Entries     int
	Checkpoints int // checkpoints verified
	Unsigned    int // entries after the newest checkpoint
	Problems    []string
}

// OK reports whether nothing was wrong.
func (r Report) OK() bool { return len(r.Problems) == 0 }

func (r *Report) problem(format string, a ...any) {
	if len(r.Problems) < 100 {
		r.Problems = append(r.Problems, fmt.Sprintf(format, a...))
	}
}

// Verify recomputes the chain and checks every checkpoint against it and
// against keys (by KeyID). A log that starts after seq 1 must start right
// after a checkpoint whose hash it chains from: that is how a pruned log
// stays verifiable.
func Verify(ctx context.Context, st Store, keys []ed25519.PublicKey) (Report, error) {
	var r Report
	byID := map[string]ed25519.PublicKey{}
	for _, k := range keys {
		byID[KeyID(k)] = k
	}
	cps, err := st.AuditCheckpoints(ctx)
	if err != nil {
		return r, err
	}
	cpAt := map[int64]store.AuditCheckpoint{}
	for _, c := range cps {
		k, ok := byID[c.KeyID]
		switch {
		case !ok:
			r.problem("checkpoint %d: signed by unknown key %s", c.Seq, c.KeyID)
		case !ed25519.Verify(k, CheckpointMessage(c.Seq, c.Hash, c.Time), c.Signature):
			r.problem("checkpoint %d: bad signature", c.Seq)
		default:
			cpAt[c.Seq] = c
		}
	}
	var prevSeq int64
	var prevHash []byte
	lastCP := int64(0)
	for from := int64(1); ; {
		es, err := st.AuditRange(ctx, from, 1000)
		if err != nil {
			return r, err
		}
		if len(es) == 0 {
			break
		}
		for _, e := range es {
			if r.Entries == 0 {
				r.First = e.Seq
				if e.Seq == 1 {
					if !isZero(e.PrevHash) {
						r.problem("entry 1: does not start from the zero hash")
					}
				} else if c, ok := cpAt[e.Seq-1]; !ok || !equal(c.Hash, e.PrevHash) {
					r.problem("entry %d: the log starts here but no valid checkpoint at %d anchors it (entries removed?)", e.Seq, e.Seq-1)
				}
			} else {
				if e.Seq != prevSeq+1 {
					r.problem("entries %d to %d are missing", prevSeq+1, e.Seq-1)
				}
				if !equal(e.PrevHash, prevHash) {
					r.problem("entry %d: does not chain from entry %d", e.Seq, prevSeq)
				}
			}
			if h := Hash(&e); !equal(h, e.Hash) {
				r.problem("entry %d: content does not match its hash (altered)", e.Seq)
			}
			if c, ok := cpAt[e.Seq]; ok {
				if !equal(c.Hash, e.Hash) {
					r.problem("checkpoint %d: signs a hash the entry does not have", e.Seq)
				} else {
					r.Checkpoints++
					lastCP = e.Seq
				}
			}
			r.Entries++
			prevSeq, prevHash = e.Seq, e.Hash
			r.Last = e.Seq
		}
		from = prevSeq + 1
	}
	// A checkpoint past the last entry signed entries that are gone. (With
	// no entry left at all, the newest checkpoint is the anchor a prune
	// leaves behind.)
	for _, c := range cps {
		if r.Entries > 0 && c.Seq > r.Last {
			r.problem("checkpoint %d: signs entries that are no longer in the log (tail removed?)", c.Seq)
		}
	}
	if r.Entries > 0 {
		r.Unsigned = int(r.Last - max(lastCP, r.First-1))
	}
	return r, nil
}

// checkpointJSON is a checkpoint as it is exported: the anchor held outside
// the database.
type checkpointJSON struct {
	Format    string    `json:"format"`
	Seq       int64     `json:"seq"`
	Hash      string    `json:"hash"`
	Time      time.Time `json:"time"`
	KeyID     string    `json:"key_id"`
	Signature string    `json:"signature"`
}

const checkpointFormat = "llmflow-audit-checkpoint/v1"

// MarshalCheckpoint is the exported form of a checkpoint.
func MarshalCheckpoint(c store.AuditCheckpoint) []byte {
	b, _ := json.MarshalIndent(checkpointJSON{checkpointFormat, c.Seq, hex.EncodeToString(c.Hash), c.Time.UTC(), c.KeyID,
		base64.StdEncoding.EncodeToString(c.Signature)}, "", "  ")
	return append(b, '\n')
}

// ParseCheckpoint reads an exported checkpoint. It checks the form only;
// VerifyAnchored checks the signature.
func ParseCheckpoint(b []byte) (store.AuditCheckpoint, error) {
	var c store.AuditCheckpoint
	if len(b) > 64<<10 {
		return c, errors.New("audit: a checkpoint larger than 64 KiB")
	}
	var j checkpointJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return c, fmt.Errorf("audit: the checkpoint is not JSON: %w", err)
	}
	if j.Format != checkpointFormat {
		return c, fmt.Errorf("audit: a checkpoint of format %q", j.Format)
	}
	h, err := hex.DecodeString(j.Hash)
	if err != nil || len(h) != sha256.Size {
		return c, errors.New("audit: the checkpoint hash is not 32 bytes of hex")
	}
	sig, err := base64.StdEncoding.DecodeString(j.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return c, errors.New("audit: the checkpoint signature is not an Ed25519 signature")
	}
	if j.Seq < 1 {
		return c, errors.New("audit: a checkpoint before the first entry")
	}
	return store.AuditCheckpoint{Seq: j.Seq, Hash: h, Time: j.Time.UTC(), KeyID: j.KeyID, Signature: sig}, nil
}

// VerifyAnchored is Verify plus a checkpoint held outside the database
// (exported to write-once storage, kept by an auditor): the database must
// still hold the anchored entry, with the anchored hash, and checkpoints at
// least as new. That is what catches what Verify alone cannot, because
// entries and checkpoints live in the same database: the tail deleted with
// the checkpoints covering it, an older snapshot restored, or everything
// deleted.
func VerifyAnchored(ctx context.Context, st Store, keys []ed25519.PublicKey, anchor store.AuditCheckpoint) (Report, error) {
	r, err := Verify(ctx, st, keys)
	if err != nil {
		return r, err
	}
	valid := false
	for _, k := range keys {
		if KeyID(k) == anchor.KeyID && ed25519.Verify(k, CheckpointMessage(anchor.Seq, anchor.Hash, anchor.Time), anchor.Signature) {
			valid = true
		}
	}
	if !valid {
		r.problem("anchor %d: its signature does not verify with the given keys", anchor.Seq)
		return r, nil
	}
	cps, err := st.AuditCheckpoints(ctx)
	if err != nil {
		return r, err
	}
	// Where the log ends: its last entry, or the checkpoint a prune left.
	end := r.Last
	if n := len(cps); n > 0 && cps[n-1].Seq > end {
		end = cps[n-1].Seq
	}
	if end < anchor.Seq {
		r.problem("anchor %d: the log ends at %d (entries removed, or an older copy restored)", anchor.Seq, end)
	}
	if n := len(cps); n == 0 || cps[n-1].Seq < anchor.Seq {
		r.problem("anchor %d: the newest checkpoint in the database is older (checkpoints removed, or an older copy restored)", anchor.Seq)
	}
	// The anchored hash must be the database's, as an entry or, once
	// pruned, as the checkpoint the prune left.
	es, err := st.AuditRange(ctx, anchor.Seq, 1)
	if err != nil {
		return r, err
	}
	switch {
	case len(es) == 1 && es[0].Seq == anchor.Seq:
		if !equal(es[0].Hash, anchor.Hash) {
			r.problem("anchor %d: the entry in the database has another hash (the log was rewritten)", anchor.Seq)
		}
	case r.First > anchor.Seq || r.Entries == 0:
		found := false
		for _, c := range cps {
			if c.Seq == anchor.Seq && equal(c.Hash, anchor.Hash) {
				found = true
			}
		}
		if !found {
			r.problem("anchor %d: pruned, and no checkpoint in the database carries its hash", anchor.Seq)
		}
	}
	return r, nil
}

// Prune deletes the oldest entries through seq, which must be a checkpoint
// covering only entries older than cutoff (for their kind): segments go
// whole, and what remains starts from a signed checkpoint. It returns the
// number of entries deleted.
func (l *Log) Prune(ctx context.Context, through int64) (int64, error) {
	cps, err := l.st.AuditCheckpoints(ctx)
	if err != nil {
		return 0, err
	}
	for _, c := range cps {
		if c.Seq == through {
			return l.st.AuditPrune(ctx, through)
		}
	}
	return 0, fmt.Errorf("audit: no checkpoint at %d to prune to", through)
}

func equal(a, b []byte) bool { return len(a) == len(b) && string(a) == string(b) }

func isZero(b []byte) bool {
	if len(b) != sha256.Size {
		return false
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
