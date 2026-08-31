// Package lxmfaddr derives a peer's LXMF delivery address from the
// public key that peer proves over a §6.7.6 LINKIDENTIFY.
//
// An RRC hub only ever sees a client while its Reticulum Link is up:
// the protocol has no store-and-forward, so a hub with something to say
// to someone who is not currently connected has nowhere to put it. LXMF
// is the store-and-forward path Reticulum does offer — a message handed
// to a propagation node waits there until the recipient's client next
// syncs.
//
// Reaching a peer that way needs an address, and the useful property
// here is that the peer does not have to register one. LINKIDENTIFY
// carries the peer's full 64-byte public key (not merely its hash), and
// every RNS destination hash is a pure function of that key and the
// destination's name — so a hub that has verified a LINKIDENTIFY can
// compute where that identity's LXMF client listens with no announce,
// no lookup, and no change to the RRC wire format.
//
// This holds only where the peer drives RRC from the same identity its
// LXMF client uses. That is true of reticulum-mobile-app, whose engine
// feeds one identity to both its RRC sessions and its lxmf.delivery
// destination, but it is an assumption about a client rather than a
// guarantee of the protocol: a client that keeps a separate RRC
// identity yields a well-formed address that nothing is listening on.
// Callers must treat delivery failure as expected, not exceptional.
package lxmfaddr

import (
	"crypto/sha256"
	"fmt"

	"github.com/thatSFguy/reticulum-go/rns"
)

// DeliveryAspect is the RNS destination full-name every LXMF client
// listens on. Its name_hash is the well-known constant
// 6ec60bc318e2c0f0d908.
const DeliveryAspect = "lxmf.delivery"

// IdentityHash returns the RNS identity hash — SHA-256(public_key)[:16]
// — for a 64-byte announced public key, or nil if pubKey is the wrong
// length. This is the value RRC carries as an envelope's K_SRC.
func IdentityHash(pubKey []byte) []byte {
	if len(pubKey) != rns.PublicKeyLen {
		return nil
	}
	h := sha256.Sum256(pubKey)
	return append([]byte(nil), h[:rns.IdentityHashLen]...)
}

// DeliveryDest returns the 16-byte lxmf.delivery destination hash for
// the identity owning pubKey — the address an LXMF message to that peer
// is sent to.
//
// The derivation is the standard one (SPEC §1.2):
//
//	identity_hash = SHA-256(public_key)[:16]
//	dest_hash     = SHA-256(name_hash("lxmf.delivery") || identity_hash)[:16]
//
// so it agrees with what the peer's own client computes for itself.
func DeliveryDest(pubKey []byte) ([]byte, error) {
	idHash := IdentityHash(pubKey)
	if idHash == nil {
		return nil, fmt.Errorf("lxmfaddr: public key must be %d bytes, got %d",
			rns.PublicKeyLen, len(pubKey))
	}
	return rns.DestinationHash(rns.NameHash(DeliveryAspect), idHash), nil
}

// KnownDelivery builds the announce-cache entry that makes a peer
// addressable for LXMF delivery, ready for rns.Transport.Restore.
//
// Restore is normally fed from a persisted announce cache, but it
// re-derives the destination hash from the public key before accepting
// an entry rather than trusting the pairing it is handed — so an entry
// synthesized from a LINKIDENTIFY key is accepted on exactly the same
// terms as one learned from a live announce, and a peer becomes
// LXMF-addressable the moment it identifies rather than whenever it
// happens to announce next.
//
// LastSeen is left zero: nothing here has heard from the peer over LXMF,
// only over RRC. Callers that want announce-cache recency semantics
// should stamp it themselves.
func KnownDelivery(pubKey []byte) (*rns.KnownIdentity, error) {
	dest, err := DeliveryDest(pubKey)
	if err != nil {
		return nil, err
	}
	return &rns.KnownIdentity{
		DestHash:  dest,
		PublicKey: append([]byte(nil), pubKey...),
		NameHash:  rns.NameHash(DeliveryAspect),
	}, nil
}
