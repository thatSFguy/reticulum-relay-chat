package hub

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// --- A3: unbounded sessions / rooms -----------------------------------

func TestMaxSessionsCap(t *testing.T) {
	h := quietHubCfg(config.HubConfig{MaxSessions: 2})
	l1 := &fakeLink{id: bytes.Repeat([]byte{0x01}, 16)}
	l2 := &fakeLink{id: bytes.Repeat([]byte{0x02}, 16)}
	l3 := &fakeLink{id: bytes.Repeat([]byte{0x03}, 16)}
	h.NewSession(l1)
	h.NewSession(l2)
	h.NewSession(l3)

	if l1.isClosed() || l2.isClosed() {
		t.Error("sessions within the cap must not be closed")
	}
	if !l3.isClosed() {
		t.Error("a session beyond MaxSessions must be refused and closed")
	}
	if got := lastError(t, l3); got != "server full" {
		t.Errorf("refused session error: got %q want %q", got, "server full")
	}
	if h.SessionCount() != 2 {
		t.Errorf("session count: got %d want 2", h.SessionCount())
	}
}

func TestMaxRoomsCap(t *testing.T) {
	h := quietHubCfg(config.HubConfig{MaxRooms: 1})
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)

	join(t, sa, idA, "room1", "") // creates room1
	join(t, sb, idB, "room2", "") // would exceed MaxRooms

	if h.RoomCount() != 1 {
		t.Errorf("room count: got %d want 1", h.RoomCount())
	}
	if got := lastError(t, linkB); got != "room limit reached" {
		t.Errorf("room-cap error: got %q want %q", got, "room limit reached")
	}
}

func TestIdleUnwelcomedSessionSwept(t *testing.T) {
	h := quietHub()
	link := &fakeLink{id: bytes.Repeat([]byte{0x07}, 16)}
	s := h.NewSession(link) // never sends HELLO — stays un-welcomed

	// Backdate creation past the idle timeout.
	s.mu.Lock()
	s.createdMs = h.now() - unwelcomedIdleTimeout.Milliseconds() - 1000
	s.mu.Unlock()

	h.doIdleSweep()
	if !link.isClosed() {
		t.Error("an idle un-welcomed session must be swept closed")
	}
}

func TestWelcomedSessionNotSwept(t *testing.T) {
	h := quietHub()
	s, link := connect(t, h, bytes.Repeat([]byte{0x08}, 16)) // welcomed

	s.mu.Lock()
	s.createdMs = h.now() - unwelcomedIdleTimeout.Milliseconds() - 1000
	s.mu.Unlock()

	h.doIdleSweep()
	if link.isClosed() {
		t.Error("a welcomed session must never be swept by the idle sweep")
	}
}

// --- A15: unbounded per-room ACL maps ---------------------------------

func TestRoomAclCap(t *testing.T) {
	h := quietHubCfg(config.HubConfig{MaxRoomAclEntries: 2})
	opID := bytes.Repeat([]byte{0x11}, 16)
	h.cfg.TrustedIdentities = []string{hex.EncodeToString(opID)}
	h.reloadTrust()

	op, opLink := connect(t, h, opID)
	join(t, op, opID, "lobby", "") // founder occupies ops slot 1

	u2 := bytes.Repeat([]byte{0x22}, 16)
	u3 := bytes.Repeat([]byte{0x33}, 16)
	connect(t, h, u2)
	connect(t, h, u3)

	// op u2 fills slot 2; op u3 must be refused.
	op.OnInbound(encode(t, clientEnvelope(rrc.TMsg, opID, "", "/op lobby "+hex.EncodeToString(u2))))
	op.OnInbound(encode(t, clientEnvelope(rrc.TMsg, opID, "", "/op lobby "+hex.EncodeToString(u3))))

	if got := lastNotice(t, opLink); !strings.Contains(got, "full") {
		t.Errorf("op beyond MaxRoomAclEntries must be refused, got %q", got)
	}
}

// --- A19: malformed-frame flood bypasses the rate limit ---------------

func TestMalformedFramesAreRateLimited(t *testing.T) {
	cfg := config.HubConfig{Limits: config.LimitsConfig{
		MaxNickBytes: 32, MaxRoomNameBytes: 64, MaxMsgBodyBytes: 4096,
		MaxRoomsPerSession: 16, RateLimitMsgsPerMin: 2,
	}}
	h := quietHubCfg(cfg)
	link := &fakeLink{id: bytes.Repeat([]byte{0x09}, 16)}
	s := h.NewSession(link)

	// Garbage that never decodes as RRC. With rate-limiting moved ahead
	// of decode (A19), each one still costs a token; the 3rd exhausts
	// the bucket of 2 and is answered with a rate-limit error.
	garbage := []byte{0xff, 0xff, 0xff, 0xff}
	s.OnInbound(garbage)
	s.OnInbound(garbage)
	s.OnInbound(garbage)

	if got := lastError(t, link); got != "rate limited" {
		t.Errorf("a flood of malformed frames must be rate limited, got %q", got)
	}
}
