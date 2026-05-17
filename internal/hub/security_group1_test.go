package hub

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// receivedNotice reports whether the link ever received a NOTICE with the
// given body text.
func receivedNotice(link *fakeLink, body string) bool {
	for _, frame := range link.frames() {
		env, err := rrc.Decode(frame)
		if err != nil {
			continue
		}
		if env.Type == rrc.TNotice && rrc.BodyText(env) == body {
			return true
		}
	}
	return false
}

// --- A1: anonymous auth/authz bypass ---------------------------------

func TestUnidentifiedPeerRejected(t *testing.T) {
	h := quietHub()
	link := &fakeLink{id: nil} // no LINKIDENTIFY ever bound
	s := h.NewSession(link)

	// The envelope K_SRC is attacker-controlled and ignored by the hub;
	// what matters is that the link carries no verified identity.
	spoofed := bytes.Repeat([]byte{0x99}, 16)
	s.OnInbound(encode(t, clientEnvelope(rrc.THello, spoofed, "", nil)))
	if s.isWelcomed() {
		t.Fatal("an un-identified peer must never be welcomed")
	}
	if got := lastError(t, link); !strings.Contains(got, "identify") {
		t.Errorf("expected an identify-required error, got %q", got)
	}

	// A JOIN from the same un-identified peer must not create a room.
	s.OnInbound(encode(t, clientEnvelope(rrc.TJoin, spoofed, "lobby", nil)))
	if h.RoomCount() != 0 {
		t.Errorf("an un-identified JOIN must not create a room (got %d)", h.RoomCount())
	}
}

func TestFounderlessRoomConfersNoOp(t *testing.T) {
	r := newRoom("lobby") // founder == ""
	if r.isOp("", false) {
		t.Error("an empty identity must never be a room operator")
	}
	if r.isVoiced("", false) {
		t.Error("an empty identity must never be voiced")
	}
}

// --- A2: ban / kline evasion -----------------------------------------

func TestBanRecheckClosesWelcomedSession(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0x44}, 16)
	s, link := connect(t, h, id)

	// The peer is welcomed, then banned mid-session (as /reload would do).
	h.mu.Lock()
	h.banned[hex.EncodeToString(id)] = struct{}{}
	h.mu.Unlock()

	s.OnInbound(encode(t, clientEnvelope(rrc.TPing, id, "", nil)))
	if !link.isClosed() {
		t.Error("a peer banned mid-session must be closed on its next frame")
	}
	if got := lastError(t, link); got != "banned" {
		t.Errorf("expected a banned error, got %q", got)
	}
}

// --- A11: RESOURCE_ENVELOPE before WELCOME ---------------------------

func TestResourceEnvelopePreWelcomeRejected(t *testing.T) {
	h := quietHubCfg(config.HubConfig{EnableResourceTransfer: true})
	id := bytes.Repeat([]byte{0x55}, 16)
	link := &fakeLink{id: id}
	s := h.NewSession(link)

	env := rrc.ResourceEnvelope(id, 1, nil, rrc.FreshID(), rrc.ResKindBlob, 10, nil, "")
	s.OnInbound(encode(t, env))

	if got := lastError(t, link); got != "send HELLO first" {
		t.Errorf("pre-WELCOME RESOURCE_ENVELOPE: got error %q want %q", got, "send HELLO first")
	}
	s.mu.Lock()
	n := len(s.expectations)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("pre-WELCOME RESOURCE_ENVELOPE registered %d expectation(s)", n)
	}
}

// --- A20: resource-delivered NOTICE bypasses room moderation ---------

func TestResourceNoticeRespectsRoomBan(t *testing.T) {
	h := quietHubCfg(config.HubConfig{EnableResourceTransfer: true})
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, linkA := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	join(t, sa, idA, "lobby", "")
	join(t, sb, idB, "lobby", "")

	// A, the founder, bans B from the room.
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "",
		"/ban lobby add "+hex.EncodeToString(idB))))

	// B smuggles a NOTICE in as a Resource instead of an inline packet.
	payload := []byte("smuggled notice")
	room := "lobby"
	env := rrc.ResourceEnvelope(idB, 1, &room, rrc.FreshID(), rrc.ResKindNotice, len(payload), nil, "")
	sb.OnInbound(encode(t, env))
	sb.OnResourceConcluded(payload)

	if got := lastError(t, linkB); got != "banned from room" {
		t.Errorf("resource NOTICE from a room-banned peer: got error %q", got)
	}
	if receivedNotice(linkA, string(payload)) {
		t.Error("a room-banned peer's resource NOTICE leaked to room members")
	}
}

// --- A13: /topic discloses private-room topics -----------------------

func TestPrivateRoomTopicHidden(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	join(t, sa, idA, "secret", "")

	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "", "/mode secret +p")))
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "", "/topic secret hush hush")))

	// B is not a member and not a server-op.
	sb.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idB, "", "/topic secret")))
	got := lastNotice(t, linkB)
	if !strings.Contains(got, "is private") {
		t.Errorf("a private room's topic must be hidden from non-members, got %q", got)
	}
	if strings.Contains(got, "hush hush") {
		t.Error("a private room's topic leaked to a non-member")
	}
}

// --- A14: short hashes accepted as ban/kline keys --------------------

func TestKlineShortHashRejected(t *testing.T) {
	h := quietHub()
	opID := bytes.Repeat([]byte{0x11}, 16)
	h.cfg.TrustedIdentities = []string{hex.EncodeToString(opID)}
	h.reloadTrust()
	op, opLink := connect(t, h, opID)

	op.OnInbound(encode(t, clientEnvelope(rrc.TMsg, opID, "", "/kline add aabbccdd")))

	if got := lastNotice(t, opLink); !strings.Contains(got, "16 bytes") {
		t.Errorf("a short (<16-byte) ban key must be rejected, got %q", got)
	}
	h.mu.Lock()
	n := len(h.klines)
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("a short hash must not be stored as a kline (got %d)", n)
	}
}
