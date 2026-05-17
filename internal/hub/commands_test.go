package hub

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// cmd sends a slash command from a session in a room and returns nothing;
// inspect the link afterwards.
func cmd(t *testing.T, s *Session, id []byte, room, body string) {
	t.Helper()
	s.OnInbound(encode(t, clientEnvelope(rrc.TMsg, id, room, body)))
}

func TestUnrecognizedCommand(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	join(t, s, id, "lobby", "")
	cmd(t, s, id, "lobby", "/frobnicate")
	if got := lastError(t, link); got != "unrecognized command" {
		t.Errorf("got %q want %q", got, "unrecognized command")
	}
}

func TestCommandNotForwarded(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	join(t, sa, idA, "lobby", "")
	join(t, sb, idB, "lobby", "")

	beforeB := len(linkB.frames())
	cmd(t, sa, idA, "lobby", "/who lobby")
	// B must not receive a MSG for the command.
	for _, f := range linkB.frames()[beforeB:] {
		env, err := rrc.Decode(f)
		if err != nil {
			continue
		}
		if env.Type == rrc.TMsg {
			t.Error("slash command was forwarded as a MSG")
		}
	}
}

func TestListRoomsRegisteredOnly(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	join(t, s, id, "lobby", "")

	// Unregistered room → not listed.
	cmd(t, s, id, "lobby", "/list")
	if got := lastNotice(t, link); got != "No public rooms registered" {
		t.Errorf("empty /list: got %q", got)
	}
}

func TestRegisterAndList(t *testing.T) {
	dir := t.TempDir()
	cfg := config.HubConfig{RoomRegistryPath: dir + "/rooms.toml"}
	h := quietHubCfg(cfg)
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	join(t, s, id, "lobby", "")

	cmd(t, s, id, "lobby", "/register lobby")
	if got := lastNotice(t, link); got != "registered room lobby" {
		t.Fatalf("/register: got %q", got)
	}
	cmd(t, s, id, "lobby", "/list")
	if got := lastNotice(t, link); !strings.Contains(got, "lobby") ||
		!strings.HasPrefix(got, "Registered public rooms:") {
		t.Errorf("/list after register: got %q", got)
	}
}

func TestRegisterRequiresFounder(t *testing.T) {
	dir := t.TempDir()
	cfg := config.HubConfig{RoomRegistryPath: dir + "/rooms.toml"}
	h := quietHubCfg(cfg)
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	join(t, sa, idA, "lobby", "") // A is founder
	join(t, sb, idB, "lobby", "")

	cmd(t, sb, idB, "lobby", "/register lobby")
	if got := lastError(t, linkB); got != "only the room founder can register" {
		t.Errorf("non-founder /register: got %q", got)
	}
}

func TestTopicViewAndSet(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	join(t, s, id, "lobby", "")

	cmd(t, s, id, "lobby", "/topic lobby")
	if got := lastNotice(t, link); got != "topic for lobby: (none)" {
		t.Errorf("topic view empty: got %q", got)
	}
	cmd(t, s, id, "lobby", "/topic lobby hello world")
	// Founder is op, allowed; broadcast NOTICE goes to the room.
	if got := lastNotice(t, link); got != "topic for lobby is now: hello world" {
		t.Errorf("topic set: got %q", got)
	}
}

func TestTopicLockedPlusT(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	join(t, sa, idA, "lobby", "")
	join(t, sb, idB, "lobby", "")

	cmd(t, sa, idA, "lobby", "/mode lobby +t")
	cmd(t, sb, idB, "lobby", "/topic lobby hijacked")
	if got := lastError(t, linkB); got != "not authorized (+t)" {
		t.Errorf("non-op topic under +t: got %q", got)
	}
}

func TestModeBroadcast(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	join(t, s, id, "lobby", "")

	cmd(t, s, id, "lobby", "/mode lobby +m")
	if got := lastNotice(t, link); got != "mode for lobby is now: +m" {
		t.Errorf("/mode +m: got %q", got)
	}
}

func TestModeModeratedEnforcement(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	join(t, sa, idA, "lobby", "") // A founder/op
	join(t, sb, idB, "lobby", "")

	cmd(t, sa, idA, "lobby", "/mode lobby +m")
	// B is not voiced → MSG rejected.
	sb.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idB, "lobby", "hi")))
	if got := lastError(t, linkB); got != "room is moderated (+m)" {
		t.Errorf("+m rejection: got %q", got)
	}
	// Voice B, then B can speak (no new ERROR frame).
	cmd(t, sa, idA, "lobby", "/voice lobby "+hex.EncodeToString(idB)[:12])
	before := len(linkB.frames())
	sb.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idB, "lobby", "now allowed")))
	for _, f := range linkB.frames()[before:] {
		env, err := rrc.Decode(f)
		if err == nil && env.Type == rrc.TError {
			t.Errorf("voiced user got ERROR %q in +m room", rrc.BodyText(env))
		}
	}
}

func TestInviteOnlyPlusI(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	join(t, sa, idA, "lobby", "")

	cmd(t, sa, idA, "lobby", "/mode lobby +i")
	join(t, sb, idB, "lobby", "")
	if got := lastError(t, linkB); got != "invite-only (+i)" {
		t.Errorf("+i rejection: got %q", got)
	}

	// Invite B, then B can join.
	cmd(t, sa, idA, "lobby", "/invite lobby add "+hex.EncodeToString(idB))
	join(t, sb, idB, "lobby", "")
	h.mu.Lock()
	r := h.rooms["lobby"]
	joined := r != nil && r.hasMemberHash(hex.EncodeToString(idB))
	h.mu.Unlock()
	if !joined {
		t.Error("invited peer must be able to join a +i room")
	}
}

func TestKeyedPlusK(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	idC := bytes.Repeat([]byte{0xC3}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	sc, _ := connect(t, h, idC)
	join(t, sa, idA, "lobby", "")

	cmd(t, sa, idA, "lobby", "/mode lobby +k s3cret")
	join(t, sb, idB, "lobby", "wrong")
	if got := lastError(t, linkB); got != "bad key (+k)" {
		t.Errorf("+k wrong key: got %q", got)
	}
	join(t, sc, idC, "lobby", "s3cret")
	h.mu.Lock()
	joined := h.rooms["lobby"].hasMemberHash(hex.EncodeToString(idC))
	h.mu.Unlock()
	if !joined {
		t.Error("correct key must allow joining a +k room")
	}
}

func TestNoOutsideMsgsPlusN(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	join(t, sa, idA, "lobby", "")

	cmd(t, sa, idA, "lobby", "/mode lobby +n")
	// B is not a member → outside MSG rejected.
	sb.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idB, "lobby", "from outside")))
	if got := lastError(t, linkB); got != "no outside messages (+n)" {
		t.Errorf("+n rejection: got %q", got)
	}
}

func TestRoomBan(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	join(t, sa, idA, "lobby", "")
	join(t, sb, idB, "lobby", "")

	cmd(t, sa, idA, "lobby", "/ban lobby add "+hex.EncodeToString(idB))
	// B must be removed from the room and told.
	if got := lastError(t, linkB); got != "banned from lobby" {
		t.Errorf("ban victim error: got %q", got)
	}
	h.mu.Lock()
	stillIn := h.rooms["lobby"].hasMemberHash(hex.EncodeToString(idB))
	h.mu.Unlock()
	if stillIn {
		t.Error("banned peer must be removed from the room")
	}
	// Re-JOIN must be refused.
	join(t, sb, idB, "lobby", "")
	if got := lastError(t, linkB); got != "banned from room" {
		t.Errorf("banned re-JOIN: got %q", got)
	}
}

func TestKick(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, linkA := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	join(t, sa, idA, "lobby", "")
	join(t, sb, idB, "lobby", "")

	cmd(t, sa, idA, "lobby", "/kick lobby "+hex.EncodeToString(idB))
	if got := lastError(t, linkB); got != "kicked from lobby" {
		t.Errorf("kick victim error: got %q", got)
	}
	if got := lastNotice(t, linkA); got != "kicked "+hex.EncodeToString(idB)+" from lobby" {
		t.Errorf("kick op notice: got %q", got)
	}
	h.mu.Lock()
	stillIn := h.rooms["lobby"].hasMemberHash(hex.EncodeToString(idB))
	h.mu.Unlock()
	if stillIn {
		t.Error("kicked peer must be removed from the room")
	}
}

func TestStatsRequiresServerOp(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	join(t, s, id, "lobby", "")
	cmd(t, s, id, "lobby", "/stats")
	if got := lastError(t, link); got != "not authorized" {
		t.Errorf("non-op /stats: got %q", got)
	}

	// Trusted identity → allowed.
	opID := bytes.Repeat([]byte{0x11}, 16)
	h.cfg.TrustedIdentities = []string{hex.EncodeToString(opID)}
	h.reloadTrust()
	op, opLink := connect(t, h, opID)
	join(t, op, opID, "lobby", "")
	cmd(t, op, opID, "lobby", "/stats")
	if got := lastNotice(t, opLink); !strings.Contains(got, "hub stats:") {
		t.Errorf("op /stats: got %q", got)
	}
}

func TestWhoListsMembers(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	join(t, s, id, "lobby", "")
	cmd(t, s, id, "lobby", "/who lobby")
	if got := lastNotice(t, link); !strings.HasPrefix(got, "members in lobby: ") {
		t.Errorf("/who: got %q", got)
	}
}

func TestModeStringOrder(t *testing.T) {
	r := newRoom("x")
	r.topicOpsOnly = true
	r.moderated = true
	r.inviteOnly = true
	if got := r.modeString(); got != "+imt" {
		t.Errorf("modeString order: got %q want +imt", got)
	}
	r2 := newRoom("y")
	if got := r2.modeString(); got != "(none)" {
		t.Errorf("empty modeString: got %q", got)
	}
}

func TestJoinRoomInfoNotice(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	join(t, s, id, "lobby", "")
	if got := lastNotice(t, link); got != "room lobby: unregistered; mode=(none); topic=(none)" {
		t.Errorf("join room-info NOTICE: got %q", got)
	}
}
