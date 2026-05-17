// Package roomreg implements TOML persistence for the RRC hub's
// registered rooms and server-wide bans (klines), mirroring the
// reference Python hub rrcd.
package roomreg

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// RoomRecord is one registered room persisted in rooms.toml.
type RoomRecord struct {
	Name          string             // room name (the TOML table key)
	Founder       string             // hex identity hash
	Topic         string             // empty = no topic
	Moderated     bool               // +m
	InviteOnly    bool               // +i
	TopicOpsOnly  bool               // +t
	NoOutsideMsgs bool               // +n
	Private       bool               // +p
	Key           string             // +k key; empty = open
	Operators     []string           // hex identity hashes
	Voiced        []string           // hex identity hashes
	Bans          []string           // hex identity hashes
	Invited       map[string]float64 // hex hash -> invite expiry, unix seconds
	LastUsedTS    float64            // unix seconds, for pruning
}

// roomDTO is the on-disk shape of a single room sub-table.
type roomDTO struct {
	Founder       string             `toml:"founder"`
	Topic         string             `toml:"topic,omitempty"`
	Moderated     bool               `toml:"moderated,omitempty"`
	InviteOnly    bool               `toml:"invite_only,omitempty"`
	TopicOpsOnly  bool               `toml:"topic_ops_only,omitempty"`
	NoOutsideMsgs bool               `toml:"no_outside_msgs,omitempty"`
	Private       bool               `toml:"private,omitempty"`
	Key           string             `toml:"key,omitempty"`
	Operators     []string           `toml:"operators,omitempty"`
	Voiced        []string           `toml:"voiced,omitempty"`
	Bans          []string           `toml:"bans,omitempty"`
	Invited       map[string]float64 `toml:"invited,omitempty"`
	LastUsedTS    float64            `toml:"last_used_ts,omitempty"`
}

// registryDTO is the on-disk shape of rooms.toml.
type registryDTO struct {
	Rooms map[string]roomDTO `toml:"rooms"`
}

// klinesDTO is the on-disk shape of the kline file.
type klinesDTO struct {
	BannedIdentities []string `toml:"banned_identities"`
}

// normHex lowercases a hex hash and strips an optional "0x" prefix.
func normHex(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "0x")
	return s
}

// normHexes normalizes a slice of hex hashes.
func normHexes(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, h := range in {
		out = append(out, normHex(h))
	}
	return out
}

// pruneInvited returns a copy of invited with expired entries removed.
// Entries with expiry <= nowUnix are dropped. Keys are normalized.
func pruneInvited(invited map[string]float64, nowUnix float64) map[string]float64 {
	if len(invited) == 0 {
		return nil
	}
	out := make(map[string]float64, len(invited))
	for h, exp := range invited {
		if exp <= nowUnix {
			continue
		}
		out[normHex(h)] = exp
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// LoadRegistry reads rooms.toml at path and returns the registered rooms
// keyed by room name. A missing file yields an empty map and no error.
// Invite entries whose expiry is in the past (relative to nowUnix) are
// dropped on load.
func LoadRegistry(path string, nowUnix float64) (map[string]*RoomRecord, error) {
	rooms := make(map[string]*RoomRecord)
	sweepStaleTemp(filepath.Dir(path)) // clear crash-orphaned temp files (A18)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return rooms, nil
		}
		return nil, fmt.Errorf("roomreg: read registry: %w", err)
	}

	var dto registryDTO
	if err := toml.Unmarshal(data, &dto); err != nil {
		return nil, fmt.Errorf("roomreg: parse registry: %w", err)
	}

	for name, rd := range dto.Rooms {
		rec := &RoomRecord{
			Name:          name,
			Founder:       normHex(rd.Founder),
			Topic:         rd.Topic,
			Moderated:     rd.Moderated,
			InviteOnly:    rd.InviteOnly,
			TopicOpsOnly:  rd.TopicOpsOnly,
			NoOutsideMsgs: rd.NoOutsideMsgs,
			Private:       rd.Private,
			Key:           rd.Key,
			Operators:     normHexes(rd.Operators),
			Voiced:        normHexes(rd.Voiced),
			Bans:          normHexes(rd.Bans),
			Invited:       pruneInvited(rd.Invited, nowUnix),
			LastUsedTS:    rd.LastUsedTS,
		}
		rooms[name] = rec
	}

	return rooms, nil
}

// SaveRegistry atomically writes the full registry to rooms.toml at path.
// Expired invites (relative to nowUnix) are dropped before writing.
// Optional/empty fields (Topic, Key, empty slices, empty Invited) are
// omitted from the output.
func SaveRegistry(path string, rooms map[string]*RoomRecord, nowUnix float64) error {
	dto := registryDTO{Rooms: make(map[string]roomDTO, len(rooms))}

	for name, rec := range rooms {
		if rec == nil {
			continue
		}
		dto.Rooms[name] = roomDTO{
			Founder:       normHex(rec.Founder),
			Topic:         rec.Topic,
			Moderated:     rec.Moderated,
			InviteOnly:    rec.InviteOnly,
			TopicOpsOnly:  rec.TopicOpsOnly,
			NoOutsideMsgs: rec.NoOutsideMsgs,
			Private:       rec.Private,
			Key:           rec.Key,
			Operators:     normHexes(rec.Operators),
			Voiced:        normHexes(rec.Voiced),
			Bans:          normHexes(rec.Bans),
			Invited:       pruneInvited(rec.Invited, nowUnix),
			LastUsedTS:    rec.LastUsedTS,
		}
	}

	return marshalAtomic(path, dto)
}

// LoadKlines reads the server-wide ban list from path. A missing file
// yields a nil slice and no error. Returned hashes are lowercased hex.
func LoadKlines(path string) ([]string, error) {
	sweepStaleTemp(filepath.Dir(path)) // clear crash-orphaned temp files (A18)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("roomreg: read klines: %w", err)
	}

	var dto klinesDTO
	if err := toml.Unmarshal(data, &dto); err != nil {
		return nil, fmt.Errorf("roomreg: parse klines: %w", err)
	}

	return normHexes(dto.BannedIdentities), nil
}

// SaveKlines atomically writes the server-wide ban list to path,
// deduplicated and sorted.
func SaveKlines(path string, hashes []string) error {
	seen := make(map[string]struct{}, len(hashes))
	uniq := make([]string, 0, len(hashes))
	for _, h := range hashes {
		n := normHex(h)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		uniq = append(uniq, n)
	}
	sort.Strings(uniq)

	return marshalAtomic(path, klinesDTO{BannedIdentities: uniq})
}

// marshalAtomic encodes v as TOML and writes it to path atomically:
// it writes to a temp file in the same directory, then renames over
// the target. Parent directories are created if needed.
func marshalAtomic(path string, v any) error {
	// Encode into memory and re-parse it before touching disk (audit
	// A7): never ship a registry/kline file the loader cannot read back
	// — e.g. one carrying an invalid-UTF-8 room name or topic, which
	// would otherwise silently wipe the registry on the next restart.
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(v); err != nil {
		return fmt.Errorf("roomreg: encode: %w", err)
	}
	var check map[string]any
	if err := toml.Unmarshal(buf.Bytes(), &check); err != nil {
		return fmt.Errorf("roomreg: refusing to write unparseable output: %w", err)
	}

	dir := filepath.Dir(path)
	// The directory holds the room registry / kline list — keep it
	// owner-only (audit A17).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("roomreg: create dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".roomreg-*.tmp")
	if err != nil {
		return fmt.Errorf("roomreg: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup if the temp file still exists.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return fmt.Errorf("roomreg: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("roomreg: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("roomreg: close temp: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("roomreg: rename: %w", err)
	}
	// fsync the parent directory so the rename itself is durable across
	// a crash (audit A18).
	fsyncDir(dir)
	return nil
}

// fsyncDir flushes a directory entry so a rename into it survives a
// crash (audit A18). Best-effort: some platforms (notably Windows) do
// not support syncing a directory handle, so a failure here is ignored.
func fsyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

// sweepStaleTemp removes orphaned .roomreg-*.tmp files left behind by a
// crash between CreateTemp and Rename (audit A18). Safe at load time,
// before any new write begins.
func sweepStaleTemp(dir string) {
	matches, err := filepath.Glob(filepath.Join(dir, ".roomreg-*.tmp"))
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}
