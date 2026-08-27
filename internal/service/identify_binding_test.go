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

// bindingTestService builds the minimal Service that handleIdentify
// touches: it reads only the identity map, its mutex, and the logger.
func bindingTestService() *Service {
	return &Service{
		log:        log.New(io.Discard, "", 0),
		sessions:   make(map[string]*hub.Session),
		identities: make(map[string]peerBinding),
	}
}

// signedIdentify builds a §6.6 LINKIDENTIFY frame in the upstream
// 128-byte form, signed the way a real client signs it.
func signedIdentify(id *rns.Identity, linkID []byte) []byte {
	pub := id.PublicKey()
	return concat(pub, id.Sign(concat(linkID, pub)))
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

	svc.handleIdentify(linkID, signedIdentify(id, linkID))

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

// A frame whose signature does not check out must bind nothing at all —
// binding the key while rejecting the hash (or vice versa) would let an
// unauthenticated peer nominate someone else's LXMF address.
func TestIdentifyWithABadSignatureBindsNothing(t *testing.T) {
	svc := bindingTestService()
	id, err := rns.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	linkID := bytes.Repeat([]byte{0xCD}, 16)

	frame := signedIdentify(id, linkID)
	frame[len(frame)-1] ^= 0xFF // corrupt the signature

	svc.handleIdentify(linkID, frame)

	if got := svc.peerIdentity(linkID); got != nil {
		t.Errorf("identity hash %x bound despite an invalid signature", got)
	}
	if got := svc.peerPublicKey(linkID); got != nil {
		t.Errorf("public key %x bound despite an invalid signature", got)
	}
}

// Signing over a different link's id must not carry over: otherwise a
// LINKIDENTIFY captured from one link could be replayed onto another.
func TestIdentifySignedForAnotherLinkIsRejected(t *testing.T) {
	svc := bindingTestService()
	id, err := rns.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	other := bytes.Repeat([]byte{0x11}, 16)
	target := bytes.Repeat([]byte{0x22}, 16)

	svc.handleIdentify(target, signedIdentify(id, other))

	if got := svc.peerPublicKey(target); got != nil {
		t.Errorf("public key %x bound from a LINKIDENTIFY signed for another link", got)
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
