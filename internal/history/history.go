// Package history stores a bounded, expiring transcript of room traffic
// so an RRC hub can tell a joining client what it missed.
//
// RRC itself keeps nothing: the hub relays a message to whoever is
// connected and forgets it. That is the protocol's stated design, and it
// is why a room reads as empty to anyone who was not present. This
// package is the hub-local answer — a per-room append log the hub can
// replay from — and it is deliberately not a database:
//
//   - One append-only file per room per UTC day. Expiry is deleting
//     whole day files, which cannot corrupt a live one and needs no
//     compaction pass.
//   - Files are opened per write rather than held. Room counts are
//     unbounded and file descriptors are not, and RRC's own per-session
//     rate limit keeps append rates far below where that would matter.
//   - No fsync. History is a convenience, and losing the last few
//     messages to a power cut is a better trade than an fsync per
//     message on the SD card of a solar-powered Pi.
//
// Everything is bounded, because a hub is an unauthenticated surface: a
// record has a size cap, a room has a byte cap, and the store has a
// total cap. Whichever binds first evicts oldest-day-first.
package history

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"
)

// Defaults applied by Open for any zero Options field.
const (
	DefaultRetention       = 7 * 24 * time.Hour
	DefaultMaxRecordBytes  = 8 * 1024
	DefaultMaxBytesPerRoom = 4 * 1024 * 1024
	DefaultMaxTotalBytes   = 128 * 1024 * 1024
)

// maxFramedRecord caps the length prefix a reader will honour, so a
// corrupt or hostile prefix cannot drive a huge allocation. It is well
// above MaxRecordBytes to leave room for framing overhead.
const maxFramedRecord = 1 << 20

// dayLayout names a day file. UTC so that a hub moving across a DST
// boundary, or misconfigured for its locale, cannot produce two files
// that sort out of order.
const dayLayout = "2006-01-02"

// ErrRecordTooLarge is returned by Append for a record whose encoded
// form exceeds MaxRecordBytes.
var ErrRecordTooLarge = errors.New("history: record exceeds the per-record size limit")

// Record is one retained room message.
//
// The fields mirror the RRC envelope that carried it, so a replay can
// reconstruct that envelope faithfully: preserving MsgID in particular
// lets a client that already saw a message recognise the replayed copy
// as the same one rather than rendering it twice.
type Record struct {
	// TimestampMs is the envelope's K_TS. It is the *sender's* clock:
	// the hub relays K_TS unchanged, so this value is client-controlled
	// and must never be trusted for expiry, ordering against server
	// time, or anything else with a security consequence. It is
	// retained because clients render it.
	TimestampMs int64 `cbor:"1,keyasint"`
	// MsgID is the envelope's K_ID (8 bytes).
	MsgID []byte `cbor:"2,keyasint"`
	// Src is the sender's 16-byte identity hash, as rewritten by the hub
	// from the link-verified identity — never the value a client claimed.
	Src []byte `cbor:"3,keyasint"`
	// Nick is the sender's advisory nickname at send time, if any.
	Nick string `cbor:"4,keyasint,omitempty"`
	// Type is the RRC message type (MSG or ACTION).
	Type int `cbor:"5,keyasint"`
	// Body is the message text.
	Body string `cbor:"6,keyasint"`

	// StoredAtMs is the hub's own clock when the record was appended.
	// Expiry and ordering use this, never TimestampMs.
	StoredAtMs int64 `cbor:"7,keyasint"`
}

// Options configures a Store. Zero fields take the package defaults.
type Options struct {
	// Retention is how long a day file is kept. Files whose day is
	// entirely older than this are deleted by Prune.
	Retention time.Duration
	// MaxRecordBytes rejects an oversized single record.
	MaxRecordBytes int
	// MaxBytesPerRoom caps one room's total on-disk size.
	MaxBytesPerRoom int64
	// MaxTotalBytes caps the whole store.
	MaxTotalBytes int64
	// Now overrides the clock, for tests.
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.Retention <= 0 {
		o.Retention = DefaultRetention
	}
	if o.MaxRecordBytes <= 0 {
		o.MaxRecordBytes = DefaultMaxRecordBytes
	}
	if o.MaxBytesPerRoom <= 0 {
		o.MaxBytesPerRoom = DefaultMaxBytesPerRoom
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Store is a room-transcript store rooted at a directory. It is safe for
// concurrent use.
type Store struct {
	dir  string
	opts Options

	mu sync.Mutex
}

// Open prepares a store rooted at dir, creating it if needed.
func Open(dir string, opts Options) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("history: directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("history: create %s: %w", dir, err)
	}
	return &Store{dir: dir, opts: opts.withDefaults()}, nil
}

// roomDir maps a room name to its directory.
//
// The name is hex-encoded rather than used as a path component. Room
// names are arbitrary client-supplied UTF-8: unencoded, a room called
// "../../etc" or one containing a separator or a NUL would escape the
// store, and on a case-insensitive filesystem two distinct rooms would
// silently share a transcript. Hex has none of those properties and is
// reversible, so the on-disk layout stays inspectable.
func (s *Store) roomDir(room string) string {
	return filepath.Join(s.dir, hex.EncodeToString([]byte(room)))
}

// RoomFromDir reverses roomDir for a directory entry name, reporting
// false for anything that is not a room directory this package wrote.
func RoomFromDir(entry string) (string, bool) {
	raw, err := hex.DecodeString(entry)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// Append records one message for a room.
//
// The day file is chosen by the hub's clock, not by the record's
// TimestampMs: that field is the sender's, and letting it pick the file
// would let any client write into an arbitrary day — backdating a
// message out of a replay window, or forward-dating one so retention
// never expires it.
func (s *Store) Append(room string, rec Record) error {
	now := s.opts.Now()
	rec.StoredAtMs = now.UnixMilli()

	blob, err := cbor.Marshal(rec)
	if err != nil {
		return fmt.Errorf("history: encode record: %w", err)
	}
	if len(blob) > s.opts.MaxRecordBytes {
		return fmt.Errorf("%w: %d > %d bytes", ErrRecordTooLarge, len(blob), s.opts.MaxRecordBytes)
	}

	framed := make([]byte, 4+len(blob))
	binary.BigEndian.PutUint32(framed[:4], uint32(len(blob)))
	copy(framed[4:], blob)

	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.roomDir(room)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("history: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, now.UTC().Format(dayLayout)+".log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("history: open %s: %w", path, err)
	}
	if _, err := f.Write(framed); err != nil {
		f.Close()
		return fmt.Errorf("history: append to %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("history: close %s: %w", path, err)
	}

	return s.enforceRoomCapLocked(dir)
}

// Query bounds a Recent call. A zero field means "no limit from this
// field"; the store's own caps still apply.
type Query struct {
	// Limit caps how many records are returned (the most recent ones).
	Limit int
	// MaxBytes caps the total body bytes returned, counted from the
	// newest backwards. A link on a slow radio is the real constraint
	// on a replay, so this is usually what binds, not Limit.
	MaxBytes int
	// Since drops records stored before this instant.
	Since time.Time
}

// Recent returns a room's most recent records, oldest first, subject to
// q and to the store's retention window.
//
// Whole day files are read to answer this. They are bounded by
// MaxBytesPerRoom, and a replay happens once per join, so the simplicity
// is worth more than the saved I/O.
func (s *Store) Recent(room string, q Query) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := s.opts.Now().Add(-s.opts.Retention)
	if q.Since.After(cutoff) {
		cutoff = q.Since
	}

	days, err := s.dayFilesLocked(s.roomDir(room))
	if err != nil {
		return nil, err
	}

	// Newest day first, stopping as soon as the caller's bounds are met,
	// so a long-lived room does not read a week of files to serve ten
	// messages.
	var collected []Record
	bytesTaken := 0
	done := false
	for i := len(days) - 1; i >= 0 && !done; i-- {
		recs, err := readDayFile(days[i].path)
		if err != nil {
			return nil, err
		}
		for j := len(recs) - 1; j >= 0; j-- {
			rec := recs[j]
			// Records within a file ascend by StoredAtMs, and files are
			// walked newest-first, so the first record past the cutoff
			// means every remaining one is too.
			if time.UnixMilli(rec.StoredAtMs).Before(cutoff) {
				done = true
				break
			}
			if q.Limit > 0 && len(collected) >= q.Limit {
				done = true
				break
			}
			// Always yield at least one record, even an oversized one:
			// returning nothing would read as "no history" rather than
			// "your budget is smaller than the last message".
			if q.MaxBytes > 0 && bytesTaken+len(rec.Body) > q.MaxBytes && len(collected) > 0 {
				done = true
				break
			}
			collected = append(collected, rec)
			bytesTaken += len(rec.Body)
		}
	}

	// Collected newest-first; callers replay oldest-first.
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return collected, nil
}

// Rooms lists the rooms that currently hold any history.
func (s *Store) Rooms() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.roomsLocked()
}

func (s *Store) roomsLocked() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("history: read %s: %w", s.dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if room, ok := RoomFromDir(e.Name()); ok {
			out = append(out, room)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Drop deletes a room's entire transcript. Used when a room is
// unregistered, or by an operator purge.
func (s *Store) Drop(room string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(s.roomDir(room)); err != nil {
		return fmt.Errorf("history: drop %s: %w", room, err)
	}
	return nil
}

// Prune deletes expired day files and enforces the store's byte caps.
// Intended to run on the hub's existing prune loop.
func (s *Store) Prune() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rooms, err := s.roomsLocked()
	if err != nil {
		return err
	}

	cutoffDay := s.opts.Now().UTC().Add(-s.opts.Retention).Format(dayLayout)
	for _, room := range rooms {
		dir := s.roomDir(room)
		days, err := s.dayFilesLocked(dir)
		if err != nil {
			return err
		}
		for _, d := range days {
			// String comparison is date comparison for this layout, and
			// a day file is dropped only once its whole day is older
			// than the window — never mid-day, so a record is never
			// deleted before its retention has actually elapsed.
			if d.day < cutoffDay {
				if err := os.Remove(d.path); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("history: expire %s: %w", d.path, err)
				}
			}
		}
		if err := s.enforceRoomCapLocked(dir); err != nil {
			return err
		}
		// An emptied room leaves its directory behind; drop it so Rooms
		// does not grow without bound as rooms come and go.
		if remaining, err := s.dayFilesLocked(dir); err == nil && len(remaining) == 0 {
			_ = os.Remove(dir)
		}
	}

	return s.enforceTotalCapLocked()
}

// dayFile is one on-disk day segment.
type dayFile struct {
	day  string // YYYY-MM-DD
	path string
	size int64
}

// dayFilesLocked lists a room's day files, oldest first. A missing
// directory is not an error — it just means the room has no history.
func (s *Store) dayFilesLocked(dir string) ([]dayFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("history: read %s: %w", dir, err)
	}
	var out []dayFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".log") {
			continue
		}
		day := strings.TrimSuffix(name, ".log")
		if _, err := time.Parse(dayLayout, day); err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, dayFile{day: day, path: filepath.Join(dir, name), size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].day < out[j].day })
	return out, nil
}

// enforceRoomCapLocked drops a room's oldest day files until it fits
// MaxBytesPerRoom. The newest day file is never dropped: a single day
// over the cap means the cap is too small for the room's traffic, and
// deleting the only file holding current conversation would leave the
// room permanently empty rather than merely short.
func (s *Store) enforceRoomCapLocked(dir string) error {
	days, err := s.dayFilesLocked(dir)
	if err != nil {
		return err
	}
	var total int64
	for _, d := range days {
		total += d.size
	}
	for i := 0; total > s.opts.MaxBytesPerRoom && i < len(days)-1; i++ {
		if err := os.Remove(days[i].path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("history: evict %s: %w", days[i].path, err)
		}
		total -= days[i].size
	}
	return nil
}

// enforceTotalCapLocked drops the oldest day files across all rooms
// until the store fits MaxTotalBytes.
func (s *Store) enforceTotalCapLocked() error {
	rooms, err := s.roomsLocked()
	if err != nil {
		return err
	}
	var all []dayFile
	var total int64
	for _, room := range rooms {
		days, err := s.dayFilesLocked(s.roomDir(room))
		if err != nil {
			return err
		}
		for _, d := range days {
			all = append(all, d)
			total += d.size
		}
	}
	if total <= s.opts.MaxTotalBytes {
		return nil
	}
	// Oldest first across every room: the cap is a property of the
	// store, so the eviction order is chronological, not per-room.
	sort.Slice(all, func(i, j int) bool { return all[i].day < all[j].day })
	for _, d := range all {
		if total <= s.opts.MaxTotalBytes {
			break
		}
		if err := os.Remove(d.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("history: evict %s: %w", d.path, err)
		}
		total -= d.size
	}
	return nil
}

// readDayFile parses one day file, oldest record first.
//
// A truncated or corrupt tail ends the read without failing the call:
// appends are not fsynced, so an unclean shutdown leaves a partial
// record as the normal case, and the records before it are still good.
func readDayFile(path string) ([]Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("history: read %s: %w", path, err)
	}
	var out []Record
	for off := 0; off+4 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[off : off+4]))
		if n <= 0 || n > maxFramedRecord || off+4+n > len(data) {
			break // truncated or corrupt — keep what parsed
		}
		var rec Record
		if err := cbor.Unmarshal(data[off+4:off+4+n], &rec); err != nil {
			break
		}
		out = append(out, rec)
		off += 4 + n
	}
	return out, nil
}
