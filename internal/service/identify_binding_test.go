package service

import (
	"bytes"
	"io"
	"log"
	"testing"

	"github.com/thatSFguy/reticulum-go/rns"
	"github.com/thatSFguy/reticulum-relay-chat/internal/hub"
	"github.com/thatSFguy/reticulum-relay-chat/internal/lxmfaddr"
)

// bindingTestService builds the minimal Service that bindPeer touches:
// it reads only the identity map, its mutex, and the logger.
func bindingTestService() *Service {
	return &Service{
		log:        log.New(io.Discard, "", 0),
		sessions:   make(map[string]*hub.Session),
		identities: make(map[string]peerBinding),
	}
}

// A verified LINKIDENTIFY must bind the peer's public key and not just
// the hash it reduces to. The hash names a peer; only the key can
// address one after the link is gone, which is what the offline
// mention path needs.
func TestIdentifyBindsThePublicKeyNotJustItsHash(t *testing.T) {
	svc := bindingTestService()
	id, err := rns.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	linkID := bytes.Repeat([]byte{0xAB}, 16)

	svc.bindPeer(linkID, id.PublicKey())

	if got := svc.peerIdentity(linkID); !bytes.Equal(got, id.Hash()) {
		t.Errorf("bound identity hash %x, want %x", got, id.Hash())
	}
	gotKey := svc.peerPublicKey(linkID)
	if !bytes.Equal(gotKey, id.PublicKey()) {
		t.Fatalf("bound public key %x, want %x", gotKey, id.PublicKey())
	}

	// The point of retaining the key: it addresses the peer's LXMF
	// client, and must agree with what that client computes for itself.
	dest, err := lxmfaddr.DeliveryDest(gotKey)
	if err != nil {
		t.Fatalf("DeliveryDest from the bound key: %v", err)
	}
	if want := id.DestinationHashFor(lxmfaddr.DeliveryAspect); !bytes.Equal(dest, want) {
		t.Errorf("bound key derives delivery destination %x, peer's own is %x", dest, want)
	}
}

// A key of the wrong size must bind nothing at all. reticulum-go
// verifies the §6.7.6 signature before calling here, so this is the one
// malformed input still reachable — and binding half of it (the hash
// but not the key, or vice versa) would leave a peer that looks
// identified but has no derivable LXMF address.
func TestBindPeerRejectsAMalformedKey(t *testing.T) {
	svc := bindingTestService()
	linkID := bytes.Repeat([]byte{0xCD}, 16)

	for _, n := range []int{0, 32, 63, 65, 128} {
		svc.bindPeer(linkID, bytes.Repeat([]byte{0x01}, n))
		if got := svc.peerIdentity(linkID); got != nil {
			t.Errorf("%d-byte key bound identity hash %x", n, got)
		}
		if got := svc.peerPublicKey(linkID); got != nil {
			t.Errorf("%d-byte key bound public key %x", n, got)
		}
	}
}

// An unidentified link has no binding at all.
func TestUnidentifiedLinkHasNoBinding(t *testing.T) {
	svc := bindingTestService()
	linkID := bytes.Repeat([]byte{0xEE}, 16)

	if got := svc.peerIdentity(linkID); got != nil {
		t.Errorf("peerIdentity = %x, want nil", got)
	}
	if got := svc.peerPublicKey(linkID); got != nil {
		t.Errorf("peerPublicKey = %x, want nil", got)
	}
}
