package peerreg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// peerFor builds a self-consistent key/identity pair.
func peerFor(seed byte, nick string) *Peer {
	key := bytes.Repeat([]byte{seed}, PublicKeyLen)
	h := sha256.Sum256(key)
	return &Peer{
		IdentityHex: hex.EncodeToString(h[:16]),
		PublicKey:   key,
		Nick:        nick,
		LastSeenTS:  1_700_000_000,
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.toml")
	alice := peerFor(0xA1, "alice")
	alice.Mentions = []Mention{
		{Room: "#lobby", ByNick: "bob", ByHex: "cafe", Text: "ping", TS: 1_700_000_100},
	}
	bob := peerFor(0xB2, "bob")
	bob.NotifyOptOut = true

	in := map[string]*Peer{alice.IdentityHex: alice, bob.IdentityHex: bob}
	if err := Save(path, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("loaded %d peers, want 2", len(out))
	}

	got := out[alice.IdentityHex]
	if got == nil {
		t.Fatal("alice is missing")
	}
	if !bytes.Equal(got.PublicKey, alice.PublicKey) {
		t.Errorf("alice's key round-tripped as %x", got.PublicKey)
	}
	if got.Nick != "alice" || got.LastSeenTS != alice.LastSeenTS {
		t.Errorf("alice round-tripped as %+v", got)
	}
	if len(got.Mentions) != 1 || got.Mentions[0].Room != "#lobby" ||
		got.Mentions[0].Text != "ping" || got.Mentions[0].ByNick != "bob" {
		t.Errorf("alice's pending mention round-tripped as %+v", got.Mentions)
	}
	if !out[bob.IdentityHex].NotifyOptOut {
		t.Error("bob's opt-out did not round-trip")
	}
}

// The key/identity pairing is what a notification is addressed from, so
// a row that does not derive must not load. Otherwise anyone who could
// write this file could redirect someone else's mentions — and the
// message text in them — to a destination of their choosing.
func TestALoadRefusesAKeyThatDoesNotDeriveItsIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.toml")
	alice := peerFor(0xA1, "alice")
	victim := peerFor(0xB2, "bob")

	// File alice's key under bob's identity.
	forged := &Peer{
		IdentityHex: victim.IdentityHex,
		PublicKey:   alice.PublicKey,
		Nick:        "bob",
	}
	if err := Save(path, map[string]*Peer{victim.IdentityHex: forged}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("loaded %d peers, want the forged row dropped", len(out))
	}
}

// One bad row must not take the whole directory down with it.
func TestOneBadRowDoesNotDiscardTheGoodOnes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.toml")
	alice := peerFor(0xA1, "alice")
	if err := Save(path, map[string]*Peer{alice.IdentityHex: alice}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Append a row with a key that is not even the right length.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString("\n[peers.00112233445566778899aabbccddeeff]\npublic_key = \"beef\"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := out[alice.IdentityHex]; !ok {
		t.Error("a valid peer was lost because of an invalid neighbour")
	}
	if len(out) != 1 {
		t.Errorf("loaded %d peers, want only the valid one", len(out))
	}
}

func TestLoadOfAMissingFileIsAnEmptyDirectory(t *testing.T) {
	out, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d peers from a missing file", len(out))
	}
}

// The file holds public keys and unread message text.
func TestTheRegistryFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.toml")
	alice := peerFor(0xA1, "alice")
	if err := Save(path, map[string]*Peer{alice.IdentityHex: alice}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("peers.toml mode is %o, want 600", perm)
	}
}

// A crash between the temp write and the rename must not leave litter
// that accumulates every run.
func TestLoadSweepsOrphanedTempFiles(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, ".peerreg-orphan.tmp")
	if err := os.WriteFile(orphan, []byte("junk"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(filepath.Join(dir, "peers.toml")); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("an orphaned temp file survived a load")
	}
}

func TestKeyMatchesIdentity(t *testing.T) {
	alice := peerFor(0xA1, "alice")
	bob := peerFor(0xB2, "bob")

	if !KeyMatchesIdentity(alice.PublicKey, alice.IdentityHex) {
		t.Error("a key did not match the identity it derives")
	}
	if KeyMatchesIdentity(alice.PublicKey, bob.IdentityHex) {
		t.Error("a key matched an identity it does not derive")
	}
	if KeyMatchesIdentity(nil, alice.IdentityHex) {
		t.Error("a nil key matched an identity")
	}
	if KeyMatchesIdentity(alice.PublicKey[:32], alice.IdentityHex) {
		t.Error("a truncated key matched an identity")
	}
	// Case and an 0x prefix are presentation, not identity.
	if !KeyMatchesIdentity(alice.PublicKey, "0x"+alice.IdentityHex) {
		t.Error("an 0x-prefixed identity did not match")
	}
}
