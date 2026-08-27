// Package peerreg is the hub's directory of the people it has met: who
// they are, how to reach them when they are not connected, and what they
// are owed.
//
// RRC needs none of this to relay a message — a link knows its own peer.
// It becomes necessary the moment the hub wants to tell someone
// something *after* their link is gone, which is what a mention
// notification is. That needs three things RRC does not keep:
//
//   - the peer's public key, so an LXMF delivery address can be derived
//     from it (see internal/lxmfaddr);
//   - a nickname bound to an identity, so "@alice" in a room resolves to
//     somebody in particular rather than to whichever session happens to
//     be advertising that nick right now;
//   - the mentions themselves, held until their recipient reappears.
//
// The file is TOML, written atomically, and mirrors internal/roomreg's
// conventions so an operator finds one kind of state file rather than
// two.
package peerreg

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/thatSFguy/reticulum-relay-chat/internal/lxmfaddr"
)

// PublicKeyLen is the length of an RNS public key: X25519 || Ed25519.
const PublicKeyLen = 64

// KeyMatchesIdentity reports whether pubKey is the key behind idHex.
//
// Checked on every load, because this pairing is what a mention
// notification is addressed from: a row whose key does not derive the
// identity it is filed under would let anyone who can write the registry
// file redirect one person's notifications — including the message text
// that prompted them — to a destination of their choosing. The check
// costs one hash and removes the question.
func KeyMatchesIdentity(pubKey []byte, idHex string) bool {
	h := lxmfaddr.IdentityHash(pubKey)
	if h == nil {
		return false
	}
	return hex.EncodeToString(h) == normHex(idHex)
}

// Peer is one identity the hub has seen.
type Peer struct {
	// IdentityHex is the 16-byte link-verified identity hash, lowercase
	// hex. It is the map key and the peer's real name here; everything
	// else is advisory.
	IdentityHex string
	// PublicKey is the 64-byte key proved over LINKIDENTIFY. Retained
	// because it, and nothing else the hub holds, yields an address that
	// works while the peer is away.
	PublicKey []byte
	// Nick is the last nickname this identity used. Advisory and
	// self-asserted: it is a convenience for resolving a mention, never
	// an authorization input.
	Nick string
	// LastSeenTS is unix seconds at the peer's most recent HELLO.
	LastSeenTS float64
	// NotifyOptOut suppresses mention notifications for this identity.
	NotifyOptOut bool
	// Mentions are notifications waiting for this peer to reappear.
	Mentions []Mention
	// NextSeq issues Mention.Seq values. In-memory only, like Seq.
	NextSeq uint64 `toml:"-"`
}

// Mention is one pending "you were named" notification.
type Mention struct {
	Room string
	// ByNick and ByHex identify who did the mentioning. Both are kept:
	// the nick is what a person recognises, the hash is what is true.
	ByNick string
	ByHex  string
	// Text is the message that carried the mention, already truncated to
	// the hub's snippet limit.
	Text string
	// TS is unix seconds when the mention happened, by the hub's clock.
	TS float64

	// Seq orders mentions within one peer's queue. In-memory only: it
	// is assigned on append (and on load, in file order), never
	// persisted, because it means nothing outside a single run.
	//
	// It exists because the queue evicts from the front when it is
	// full, so a mention's index is not stable across the seconds an
	// offline push spends in flight. Removing what was sent by index
	// would discard whatever arrived meanwhile; removing it by Seq
	// cannot.
	Seq uint64 `toml:"-"`
}

// peerDTO is the on-disk shape of one peer.
type peerDTO struct {
	PublicKey    string       `toml:"public_key"`
	Nick         string       `toml:"nick,omitempty"`
	LastSeenTS   float64      `toml:"last_seen_ts,omitempty"`
	NotifyOptOut bool         `toml:"notify_opt_out,omitempty"`
	Mentions     []mentionDTO `toml:"mentions,omitempty"`
}

type mentionDTO struct {
	Room   string  `toml:"room"`
	ByNick string  `toml:"by_nick,omitempty"`
	ByHex  string  `toml:"by_hex,omitempty"`
	Text   string  `toml:"text"`
	TS     float64 `toml:"ts,omitempty"`
}

type registryDTO struct {
	Peers map[string]peerDTO `toml:"peers"`
}

// Load reads the peer directory. A missing file is an empty directory,
// not an error — a hub's first run has met nobody.
//
// Entries that do not survive validation are dropped rather than
// failing the load: a peer whose key does not match its identity hash is
// unusable and possibly planted, and refusing to start over one corrupt
// row would take the hub down for a file it can simply repair.
func Load(path string) (map[string]*Peer, error) {
	if path == "" {
		return map[string]*Peer{}, nil
	}
	sweepStaleTemp(filepath.Dir(path))

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]*Peer{}, nil
		}
		return nil, fmt.Errorf("peerreg: read %s: %w", path, err)
	}
	var dto registryDTO
	if err := toml.Unmarshal(data, &dto); err != nil {
		return nil, fmt.Errorf("peerreg: parse %s: %w", path, err)
	}

	out := make(map[string]*Peer, len(dto.Peers))
	for idHex, p := range dto.Peers {
		id := normHex(idHex)
		key, err := hex.DecodeString(strings.TrimSpace(p.PublicKey))
		if err != nil || len(key) != PublicKeyLen {
			continue
		}
		if !KeyMatchesIdentity(key, id) {
			continue
		}
		peer := &Peer{
			IdentityHex:  id,
			PublicKey:    key,
			Nick:         p.Nick,
			LastSeenTS:   p.LastSeenTS,
			NotifyOptOut: p.NotifyOptOut,
		}
		for _, m := range p.Mentions {
			peer.NextSeq++
			peer.Mentions = append(peer.Mentions, Mention{
				Room:   m.Room,
				ByNick: m.ByNick,
				ByHex:  m.ByHex,
				Text:   m.Text,
				TS:     m.TS,
				Seq:    peer.NextSeq,
			})
		}
		out[id] = peer
	}
	return out, nil
}

// Save writes the peer directory atomically.
func Save(path string, peers map[string]*Peer) error {
	if path == "" {
		return nil
	}
	dto := registryDTO{Peers: make(map[string]peerDTO, len(peers))}
	for id, p := range peers {
		if p == nil || len(p.PublicKey) != PublicKeyLen {
			continue
		}
		row := peerDTO{
			PublicKey:    hex.EncodeToString(p.PublicKey),
			Nick:         p.Nick,
			LastSeenTS:   p.LastSeenTS,
			NotifyOptOut: p.NotifyOptOut,
		}
		for _, m := range p.Mentions {
			row.Mentions = append(row.Mentions, mentionDTO{
				Room:   m.Room,
				ByNick: m.ByNick,
				ByHex:  m.ByHex,
				Text:   m.Text,
				TS:     m.TS,
			})
		}
		dto.Peers[normHex(id)] = row
	}
	return marshalAtomic(path, dto)
}

// SortedIdentities returns the directory's identity hashes in a stable
// order, so callers iterating for display or eviction do not depend on
// Go's randomized map order.
func SortedIdentities(peers map[string]*Peer) []string {
	out := make([]string, 0, len(peers))
	for id := range peers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func normHex(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	return strings.TrimPrefix(s, "0x")
}

// marshalAtomic encodes v to TOML and replaces path with it in one
// rename, mirroring internal/roomreg: encode to memory, re-parse it
// before touching disk so an unreadable file is never shipped, write a
// temp file in the destination directory, fsync it, rename, then fsync
// the directory so the rename survives a crash.
func marshalAtomic(path string, v any) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(v); err != nil {
		return fmt.Errorf("peerreg: encode: %w", err)
	}
	var check map[string]any
	if err := toml.Unmarshal(buf.Bytes(), &check); err != nil {
		return fmt.Errorf("peerreg: refusing to write unparseable output: %w", err)
	}

	dir := filepath.Dir(path)
	// The directory holds public keys and pending message text — keep it
	// owner-only, as roomreg does.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("peerreg: create dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".peerreg-*.tmp")
	if err != nil {
		return fmt.Errorf("peerreg: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return fmt.Errorf("peerreg: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("peerreg: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("peerreg: close temp: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("peerreg: chmod temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("peerreg: rename: %w", err)
	}
	fsyncDir(dir)
	return nil
}

func fsyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

// sweepStaleTemp removes temp files orphaned by a crash between
// CreateTemp and Rename.
func sweepStaleTemp(dir string) {
	matches, err := filepath.Glob(filepath.Join(dir, ".peerreg-*.tmp"))
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}
