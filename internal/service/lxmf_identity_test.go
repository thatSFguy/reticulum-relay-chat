package service

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/thatSFguy/reticulum-go/lxmf"
	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
)

// notifyingConfig is a hub with the LXMF notification path on, with
// every file it writes inside dir.
func notifyingConfig(dir string) config.HubConfig {
	h := config.DefaultsForTest()
	h.Name = "test-hub"
	h.IdentityPath = filepath.Join(dir, "hub_identity")
	h.PeerRegistryPath = filepath.Join(dir, "peers.toml")
	h.RoomRegistryPath = filepath.Join(dir, "rooms.toml")
	h.MentionNotify = true
	h.MentionLXMF = true
	return h
}

func newTestService(t *testing.T, h config.HubConfig) *Service {
	t.Helper()
	svc, err := New(&config.Config{Hub: h}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

// The two destinations must not share a key.
//
// Their destination HASHES always differed — rrc.hub and lxmf.delivery
// are different aspects — but both used to derive from the hub identity,
// so one public key and one identity hash stood behind both announces. A
// client that stores what it hears keyed by identity rather than by
// destination collapses those into a single entry, and the later
// announce overwrites the earlier one: the hub stops being somewhere you
// can join a room and becomes a "(noreply)" contact. Nothing on the
// sending side can see that happen (SPEC §4.5 announces are
// unacknowledged), so the split is asserted here instead.
func TestTheNotificationIdentityIsNotTheHubIdentity(t *testing.T) {
	svc := newTestService(t, notifyingConfig(t.TempDir()))

	if svc.lxmfIdentity == nil {
		t.Fatal("no notification identity — the notifier did not start")
	}
	if bytes.Equal(svc.identity.Hash(), svc.lxmfIdentity.Hash()) {
		t.Errorf("both destinations hang off identity %s; that shared key is the whole bug",
			svc.identity.HexHash())
	}
	// The specific value a client keying by identity would collide on.
	if shared := svc.identity.DestinationHashFor(lxmf.FullName()); bytes.Equal(shared, svc.lxmfDest) {
		t.Errorf("lxmf.delivery is still %x, derived from the hub identity", shared)
	}
	if bytes.Equal(svc.destHash, svc.lxmfDest) {
		t.Errorf("rrc.hub and lxmf.delivery are the same destination %x", svc.destHash)
	}
}

// The notification address must survive a restart. It is a sender
// address recipients see and reply to, and a hub that regenerates the
// key on every start is a new stranger in everyone's contact list each
// time — the reason the identity is persisted rather than kept in
// memory for the lifetime of the process.
func TestTheNotificationIdentityIsStableAcrossRestarts(t *testing.T) {
	h := notifyingConfig(t.TempDir())
	first := newTestService(t, h)
	second := newTestService(t, h)

	if !bytes.Equal(first.lxmfDest, second.lxmfDest) {
		t.Errorf("notification address changed on restart: %x -> %x", first.lxmfDest, second.lxmfDest)
	}
	if _, err := os.Stat(h.LXMFIdentityFile()); err != nil {
		t.Errorf("identity was not saved to %s: %v", h.LXMFIdentityFile(), err)
	}
}

// A hub that does not notify has no notification address, and must not
// be left holding a private key for one it never announces.
func TestNoNotificationIdentityWhenTheFeatureIsOff(t *testing.T) {
	h := notifyingConfig(t.TempDir())
	h.MentionLXMF = false
	svc := newTestService(t, h)

	if svc.lxmfIdentity != nil || svc.lxmfDest != nil {
		t.Errorf("built a notification identity for a hub with mention_lxmf off")
	}
	if _, err := os.Stat(h.LXMFIdentityFile()); !os.IsNotExist(err) {
		t.Errorf("wrote %s anyway (err=%v)", h.LXMFIdentityFile(), err)
	}
}
