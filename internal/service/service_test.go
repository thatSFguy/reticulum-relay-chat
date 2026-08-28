package service

import (
	"bytes"
	"testing"

	"github.com/thatSFguy/reticulum-go/rns"
)

// The §6.7.6 frame layout is parsed and verified inside reticulum-go
// now (see the note above bindPeer), so RRC no longer slices it. What
// still has to hold at this boundary is the shape RRC depends on: the
// upstream 128-byte body is public_key(64) || signature(64), and the
// signature covers link_id || public_key — NOT link_id alone, which is
// the mistake §6.7.6 calls out by name.
//
// Pinned here rather than left to the library because it is the
// contract RRC's peer binding rests on: a change to it would silently
// stop every client identifying, and the hub reaps un-welcomed
// sessions without a word about why.
func TestLinkIdentifyFrameContract(t *testing.T) {
	id, err := rns.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	linkID := bytes.Repeat([]byte{0xAB}, rns.IdentityHashLen)
	pub := id.PublicKey()

	if rns.LinkIdentifyBodyLen != 128 {
		t.Errorf("LINKIDENTIFY body is %d bytes, want the 128-byte spec form",
			rns.LinkIdentifyBodyLen)
	}

	sig := id.Sign(concat(linkID, pub))
	if !rns.VerifyLinkIdentify(linkID, pub, sig) {
		t.Fatal("a signature over link_id || public_key must verify")
	}

	t.Run("signing link_id alone is rejected", func(t *testing.T) {
		if rns.VerifyLinkIdentify(linkID, pub, id.Sign(linkID)) {
			t.Error("§6.7.6 requires the public key in the signed data")
		}
	})

	t.Run("a signature for another link is rejected", func(t *testing.T) {
		other := bytes.Repeat([]byte{0x11}, rns.IdentityHashLen)
		if rns.VerifyLinkIdentify(linkID, pub, id.Sign(concat(other, pub))) {
			t.Error("a LINKIDENTIFY captured on one link must not replay onto another")
		}
	})

	t.Run("a corrupt signature is rejected", func(t *testing.T) {
		bad := append([]byte(nil), sig...)
		bad[len(bad)-1] ^= 0xFF
		if rns.VerifyLinkIdentify(linkID, pub, bad) {
			t.Error("a corrupted signature must not verify")
		}
	})
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
