// Package spool is the gateway's durable accounting spool: an append-only
// file per process where every batch of usage records is written, and
// fsync'd, before it is inserted into the store. A store outage or a crash
// then loses nothing that reached the spool: the records are inserted when
// the store is back, or by the next process that starts on the same
// directory. Inserts are idempotent (keyed by the record ID), so replaying
// a batch that was already written before a crash bills nothing twice.
//
// A spool file is a sequence of frames:
//
//	magic 'L' 'F' | length uint32 BE | CRC-32C of the payload, uint32 BE | payload
//
// whose payload is one JSON-encoded store.Record. After a crash the file is
// untrusted input: a torn tail (an incomplete last frame) is expected and
// ignored; anything else that does not check out (bad magic, bad CRC, a
// length beyond MaxFrame, a payload that is not a record) stops the reading
// there and marks the file corrupt, kept for an operator instead of deleted.
package spool

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/go-llmflow/gateway/store"
)

// MaxFrame bounds one frame's payload: a usage record is a few hundred
// bytes, tags included.
const MaxFrame = 64 << 10

const headerSize = 10

var (
	magic  = [2]byte{'L', 'F'}
	crcTab = crc32.MakeTable(crc32.Castagnoli)

	// ErrFull is returned by Append when the batch would take the spool
	// beyond its bound.
	ErrFull = errors.New("spool: full")
)

// Tail says how a file ended.
type Tail int

const (
	// Clean: the file ends at a frame boundary.
	Clean Tail = iota
	// Torn: the last frame is incomplete (a write cut by a crash).
	Torn
	// Corrupt: a frame does not check out; what follows was not read.
	Corrupt
)

func (t Tail) String() string { return [...]string{"clean", "torn", "corrupt"}[t] }

// Encode appends the frames of recs to dst.
func Encode(dst []byte, recs []store.Record) ([]byte, error) {
	for i := range recs {
		p, err := json.Marshal(&recs[i])
		if err != nil {
			return dst, err
		}
		if len(p) > MaxFrame {
			return dst, fmt.Errorf("spool: a record of %d bytes", len(p))
		}
		var h [headerSize]byte
		h[0], h[1] = magic[0], magic[1]
		binary.BigEndian.PutUint32(h[2:], uint32(len(p)))
		binary.BigEndian.PutUint32(h[6:], crc32.Checksum(p, crcTab))
		dst = append(append(dst, h[:]...), p...)
	}
	return dst, nil
}

// Read decodes frames from r until its end, a torn tail or a corrupt frame.
// It never allocates more than MaxFrame for one frame, whatever r holds.
func Read(r io.Reader) ([]store.Record, Tail, error) {
	br := bufio.NewReader(r)
	var out []store.Record
	buf := make([]byte, 0, 1024)
	for {
		var h [headerSize]byte
		_, err := io.ReadFull(br, h[:])
		switch {
		case err == io.EOF:
			return out, Clean, nil
		case err == io.ErrUnexpectedEOF:
			return out, Torn, nil
		case err != nil:
			return out, Clean, err
		}
		if h[0] != magic[0] || h[1] != magic[1] {
			return out, Corrupt, nil
		}
		size := binary.BigEndian.Uint32(h[2:])
		if size > MaxFrame {
			return out, Corrupt, nil
		}
		if cap(buf) < int(size) {
			buf = make([]byte, size)
		}
		buf = buf[:size]
		if _, err := io.ReadFull(br, buf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return out, Torn, nil
			}
			return out, Clean, err
		}
		if crc32.Checksum(buf, crcTab) != binary.BigEndian.Uint32(h[6:]) {
			return out, Corrupt, nil
		}
		var rec store.Record
		dec := json.NewDecoder(bytes.NewReader(buf))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rec); err != nil || rec.ID == "" {
			// A checked frame that is not a record: written by something
			// else, or by a future version. Not ours to bill.
			return out, Corrupt, nil
		}
		out = append(out, rec)
	}
}

const (
	prefix = "acct-"
	suffix = ".spool"
)

// Spool is one process's spool file.
type Spool struct {
	mu   sync.Mutex
	dir  string
	path string
	f    *os.File
	size int64
	max  int64
}

// Open creates dir (0700) if needed and a new spool file in it (0600),
// locked for this process.
func Open(dir string, max int64) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("spool: %s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("spool: %s is writable by others (mode %o): another user could plant records", dir, fi.Mode().Perm())
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, prefix+hex.EncodeToString(b[:])+suffix)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	if err := lock(f, true); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("spool: lock: %w", err)
	}
	return &Spool{dir: dir, path: path, f: f, max: max}, nil
}

// Path is the spool file's path.
func (s *Spool) Path() string { return s.path }

// Size is the bytes the spool holds.
func (s *Spool) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// Append writes recs and fsyncs; ErrFull when they do not fit.
func (s *Spool) Append(recs []store.Record) error {
	b, err := Encode(nil, recs)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.size+int64(len(b)) > s.max {
		return ErrFull
	}
	n, err := s.f.Write(b)
	s.size += int64(n)
	if err != nil {
		return err
	}
	return s.f.Sync()
}

// Reset empties the spool: everything it held is stored.
func (s *Spool) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.size == 0 {
		return nil
	}
	if err := s.f.Truncate(0); err != nil {
		return err
	}
	s.size = 0
	return s.f.Sync()
}

// Close closes the file, removing it when empty; a file that still holds
// records stays for the next process to replay.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	empty := s.size == 0
	err := s.f.Close()
	if empty {
		os.Remove(s.path)
	}
	return err
}

// Orphan is a spool file left by a process that is gone.
type Orphan struct {
	Path    string
	Records []store.Record
	Tail    Tail
	f       *os.File
}

// Done removes a replayed orphan (renamed *.corrupt when its tail was
// corrupt, so an operator can look at what could not be read).
func (o *Orphan) Done() error {
	defer o.f.Close()
	if o.Tail == Corrupt {
		return os.Rename(o.Path, o.Path+".corrupt")
	}
	return os.Remove(o.Path)
}

// Release gives an orphan back, unreplayed, for a later attempt.
func (o *Orphan) Release() { o.f.Close() }

// Orphans returns the spool files of dir that no live process holds (their
// lock is free), except skip, read and locked for the caller. A file that
// is not a regular file of this user's making is left alone.
func Orphans(dir, skip string, maxBytes int64) ([]*Orphan, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	var out []*Orphan
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		path := filepath.Join(dir, name)
		if path == skip {
			continue
		}
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
			continue
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		if lock(f, false) != nil {
			f.Close() // a live process holds it
			continue
		}
		recs, tail, err := Read(io.LimitReader(f, maxBytes))
		if err != nil {
			f.Close()
			continue
		}
		out = append(out, &Orphan{Path: path, Records: recs, Tail: tail, f: f})
	}
	return out, nil
}
