package hub

import (
	"bytes"
	"testing"
)

// A session must surface the peer key its link verified, unchanged: the
// mention-notification path derives an LXMF delivery address from that
// key, and a truncated or substituted one addresses some other
// destination silently rather than failing.
func TestSessionSurfacesTheLinksVerifiedPeerKey(t *testing.T) {
	h := quietHub()
	key := bytes.Repeat([]byte{0xAB}, 64)
	s := h.NewSession(&fakeLink{id: bytes.Repeat([]byte{0xA1}, 16), pubKey: key})

	if got := s.PeerPublicKey(); !bytes.Equal(got, key) {
		t.Errorf("PeerPublicKey() = %x, want %x", got, key)
	}
}

// A client that has not sent LINKIDENTIFY has no key, and the hub must
// report that rather than inventing one.
func TestSessionReportsNoKeyForAnUnidentifiedPeer(t *testing.T) {
	h := quietHub()
	s := h.NewSession(&fakeLink{id: bytes.Repeat([]byte{0xA2}, 16)})

	if got := s.PeerPublicKey(); got != nil {
		t.Errorf("PeerPublicKey() = %x for an unidentified peer, want nil", got)
	}
}
