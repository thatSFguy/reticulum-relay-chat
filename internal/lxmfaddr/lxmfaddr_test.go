package lxmfaddr

import (
	"bytes"
	"encoding/hex"
	"io"
	"log"
	"testing"

	"github.com/thatSFguy/reticulum-go/rns"
)

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// The whole scheme rests on the hub computing, from a public key alone,
// the same address the key's owner computes for itself. Derive it both
// ways for one identity: ours from the raw public key (all a hub gets
// from a LINKIDENTIFY), theirs from the identity object.
func TestDeliveryDestMatchesTheOwnersOwnDerivation(t *testing.T) {
	id, err := rns.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}

	got, err := DeliveryDest(id.PublicKey())
	if err != nil {
		t.Fatalf("DeliveryDest: %v", err)
	}
	want := id.DestinationHashFor(DeliveryAspect)
	if !bytes.Equal(got, want) {
		t.Errorf("derived %x from the public key, but the identity's own destination is %x",
			got, want)
	}
}

func TestIdentityHashMatchesTheIdentitysOwnHash(t *testing.T) {
	id, err := rns.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	if got, want := IdentityHash(id.PublicKey()), id.Hash(); !bytes.Equal(got, want) {
		t.Errorf("IdentityHash = %x, identity reports %x", got, want)
	}
}

// Transport.Restore re-derives the destination hash from the public key
// and refuses entries that don't check out, so a successful Recall is
// rns itself confirming our derivation — not just our own arithmetic
// agreeing with itself.
func TestKnownDeliveryIsAcceptedByTransportRestore(t *testing.T) {
	id, err := rns.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	known, err := KnownDelivery(id.PublicKey())
	if err != nil {
		t.Fatalf("KnownDelivery: %v", err)
	}

	tr := rns.NewTransport(quietLogger())
	tr.Restore(known)

	recalled := tr.Recall(known.DestHash)
	if recalled == nil {
		t.Fatal("transport refused the synthesized entry — the destination hash does not derive from the public key")
	}
	if !bytes.Equal(recalled.PublicKey, id.PublicKey()) {
		t.Errorf("recalled public key %x, want %x", recalled.PublicKey, id.PublicKey())
	}
}

// A tampered pairing must not become addressable: swapping in another
// identity's key leaves the destination hash no longer derivable from
// it, which is exactly what Restore checks.
func TestTransportRefusesAMismatchedPairing(t *testing.T) {
	victim, err := rns.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	attacker, err := rns.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	known, err := KnownDelivery(victim.PublicKey())
	if err != nil {
		t.Fatalf("KnownDelivery: %v", err)
	}
	known.PublicKey = attacker.PublicKey()

	tr := rns.NewTransport(quietLogger())
	tr.Restore(known)

	if tr.Recall(known.DestHash) != nil {
		t.Error("transport accepted a destination bound to the wrong public key")
	}
}

func TestDeliveryAspectNameHashIsTheWellKnownConstant(t *testing.T) {
	const want = "6ec60bc318e2c0f0d908"
	if got := hex.EncodeToString(rns.NameHash(DeliveryAspect)); got != want {
		t.Errorf("name_hash(%q) = %s, want %s", DeliveryAspect, got, want)
	}
}

func TestWrongLengthKeysAreRejected(t *testing.T) {
	for _, n := range []int{0, 16, 32, 63, 65, 128} {
		if _, err := DeliveryDest(make([]byte, n)); err == nil {
			t.Errorf("DeliveryDest accepted a %d-byte key", n)
		}
		if _, err := KnownDelivery(make([]byte, n)); err == nil {
			t.Errorf("KnownDelivery accepted a %d-byte key", n)
		}
		if got := IdentityHash(make([]byte, n)); got != nil {
			t.Errorf("IdentityHash(%d bytes) = %x, want nil", n, got)
		}
	}
}

// The returned slices must not alias the caller's key, or a later
// in-place edit of one would silently corrupt the other.
func TestReturnedSlicesDoNotAliasTheInput(t *testing.T) {
	id, err := rns.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	pub := append([]byte(nil), id.PublicKey()...)
	known, err := KnownDelivery(pub)
	if err != nil {
		t.Fatalf("KnownDelivery: %v", err)
	}
	pub[0] ^= 0xff
	if bytes.Equal(known.PublicKey, pub) {
		t.Error("KnownDelivery aliased the caller's key slice")
	}
}
