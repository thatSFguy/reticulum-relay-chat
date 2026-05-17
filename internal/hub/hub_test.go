package hub

import (
	"bytes"
	"encoding/hex"
	"io"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// fakeLink is an in-memory hub.Link that records every frame sent to it.
type fakeLink struct {
	id          []byte
	mu          sync.Mutex
	sent        [][]byte
	resources   [][]byte
	closed      bool
	noResources bool // when set, SendResource returns an error
}

func (f *fakeLink) Send(frame []byte) error {
	f.mu.Lock()
	f.sent = append(f.sent, append([]byte(nil), frame...))
	f.mu.Unlock()
	return nil
}

func (f *fakeLink) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

func (f *fakeLink) PeerIdentityHash() []byte { return f.id }

func (f *fakeLink) SendResource(payload []byte) error {
	if f.noResources {
		return io.ErrClosedPipe
	}
	f.mu.Lock()
	f.resources = append(f.resources, append([]byte(nil), payload...))
	f.mu.Unlock()
	return nil
}

func (f *fakeLink) frames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeLink) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// --- harness ---------------------------------------------------------

func quietHubCfg(cfg config.HubConfig) *Hub {
	hubID := bytes.Repeat([]byte{0xFF}, 16)
	if cfg.Name == "" {
		cfg.Name = "Test Hub"
	}
	if cfg.Version == "" {
		cfg.Version = "test"
	}
	if cfg.Limits == (config.LimitsConfig{}) {
		cfg.Limits = config.LimitsConfig{
			MaxNickBytes: 32, MaxRoomNameBytes: 64, MaxMsgBodyBytes: 4096,
			MaxRoomsPerSession: 16, RateLimitMsgsPerMin: 240,
		}
	}
	return New(hubID, cfg, log.New(io.Discard, "", 0))
}

func quietHub() *Hub { return quietHubCfg(config.HubConfig{}) }

func encode(t *testing.T, e *rrc.Envelope) []byte {
	t.Helper()
	b, err := e.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

func clientEnvelope(typ int, src []byte, room string, body any) *rrc.Envelope {
	e := &rrc.Envelope{
		Version:     rrc.Version,
		Type:        typ,
		MsgID:       rrc.FreshID(),
		TimestampMs: 1,
		Src:         src,
		Body:        body,
	}
	if room != "" {
		e.Room = &room
	}
	return e
}

// connect creates a session, sends HELLO, and returns it.
func connect(t *testing.T, h *Hub, id []byte) (*Session, *fakeLink) {
	t.Helper()
	link := &fakeLink{id: id}
	s := h.NewSession(link)
	s.OnInbound(encode(t, clientEnvelope(rrc.THello, id, "", nil)))
	return s, link
}

// join sends a JOIN for room with an optional key body.
func join(t *testing.T, s *Session, id []byte, room, key string) {
	t.Helper()
	var body any
	if key != "" {
		body = key
	}
	s.OnInbound(encode(t, clientEnvelope(rrc.TJoin, id, room, body)))
}

// lastNotice returns the most recent NOTICE body on a link.
func lastNotice(t *testing.T, link *fakeLink) string {
	t.Helper()
	frames := link.frames()
	for i := len(frames) - 1; i >= 0; i-- {
		env, err := rrc.Decode(frames[i])
		if err != nil {
			continue
		}
		if env.Type == rrc.TNotice {
			return rrc.BodyText(env)
		}
	}
	return ""
}

// lastError returns the most recent ERROR body on a link.
func lastError(t *testing.T, link *fakeLink) string {
	t.Helper()
	frames := link.frames()
	for i := len(frames) - 1; i >= 0; i-- {
		env, err := rrc.Decode(frames[i])
		if err != nil {
			continue
		}
		if env.Type == rrc.TError {
			return rrc.BodyText(env)
		}
	}
	return ""
}

func lastTypeBody(t *testing.T, link *fakeLink, typ int) (*rrc.Envelope, bool) {
	t.Helper()
	frames := link.frames()
	for i := len(frames) - 1; i >= 0; i-- {
		env, err := rrc.Decode(frames[i])
		if err != nil {
			continue
		}
		if env.Type == typ {
			return env, true
		}
	}
	return nil, false
}

// --- basic routing ----------------------------------------------------

func TestMessageFanout(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	join(t, sa, idA, "lobby", "")
	join(t, sb, idB, "lobby", "")

	if h.RoomCount() != 1 {
		t.Fatalf("expected 1 room, got %d", h.RoomCount())
	}

	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "lobby", "hi there")))
	relayed, ok := lastTypeBody(t, linkB, rrc.TMsg)
	if !ok {
		t.Fatal("link B received no MSG")
	}
	if rrc.BodyText(relayed) != "hi there" {
		t.Errorf("relayed body: got %q", rrc.BodyText(relayed))
	}
	if !bytes.Equal(relayed.Src, idA) {
		t.Errorf("relayed K_SRC: got %x want %x", relayed.Src, idA)
	}
}

func TestWelcomeGate(t *testing.T) {
	h := quietHub()
	link := &fakeLink{id: bytes.Repeat([]byte{0x0C}, 16)}
	s := h.NewSession(link)

	s.OnInbound(encode(t, clientEnvelope(rrc.TJoin, link.id, "lobby", nil)))
	if h.RoomCount() != 0 {
		t.Errorf("a pre-HELLO JOIN must not create a room (got %d)", h.RoomCount())
	}
	if got := lastError(t, link); got != "send HELLO first" {
		t.Errorf("pre-HELLO JOIN error: got %q want %q", got, "send HELLO first")
	}
}

func TestPingPong(t *testing.T) {
	h := quietHub()
	s, link := connect(t, h, bytes.Repeat([]byte{0x0D}, 16))
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	s.OnInbound(encode(t, clientEnvelope(rrc.TPing, link.id, "", payload)))
	pong, ok := lastTypeBody(t, link, rrc.TPong)
	if !ok {
		t.Fatal("no PONG")
	}
	if !bytes.Equal(rrc.BodyBytes(pong), payload) {
		t.Errorf("PONG payload: got %x want %x", rrc.BodyBytes(pong), payload)
	}
}

func TestActionRouting(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	join(t, sa, idA, "lobby", "")
	join(t, sb, idB, "lobby", "")

	// An ACTION body that starts with "/" must NOT be command-dispatched.
	sa.OnInbound(encode(t, clientEnvelope(rrc.TAction, idA, "lobby", "/me waves")))
	act, ok := lastTypeBody(t, linkB, rrc.TAction)
	if !ok {
		t.Fatal("ACTION was not routed to room members")
	}
	if rrc.BodyText(act) != "/me waves" {
		t.Errorf("ACTION body: got %q", rrc.BodyText(act))
	}
	if !bytes.Equal(act.Src, idA) {
		t.Errorf("ACTION src not rewritten: %x", act.Src)
	}
}

func TestReHelloReset(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, _ := connect(t, h, id)
	join(t, s, id, "lobby", "")
	if h.RoomCount() != 1 {
		t.Fatalf("expected room after join")
	}
	s.mu.Lock()
	joinedCount := len(s.joined)
	s.mu.Unlock()
	if joinedCount != 1 {
		t.Fatalf("expected 1 joined room, got %d", joinedCount)
	}

	// Re-HELLO must remove the peer from all rooms.
	s.OnInbound(encode(t, clientEnvelope(rrc.THello, id, "", nil)))
	s.mu.Lock()
	joinedCount = len(s.joined)
	s.mu.Unlock()
	if joinedCount != 0 {
		t.Errorf("re-HELLO must clear joined rooms, got %d", joinedCount)
	}
	if h.RoomCount() != 0 {
		t.Errorf("re-HELLO must drop the now-empty room, got %d", h.RoomCount())
	}
}

// --- rate limiting ----------------------------------------------------

func TestTokenBucketRateLimit(t *testing.T) {
	cfg := config.HubConfig{Limits: config.LimitsConfig{
		MaxNickBytes: 32, MaxRoomNameBytes: 64, MaxMsgBodyBytes: 4096,
		MaxRoomsPerSession: 16, RateLimitMsgsPerMin: 5,
	}}
	h := quietHubCfg(cfg)
	id := bytes.Repeat([]byte{0xA1}, 16)
	link := &fakeLink{id: id}
	s := h.NewSession(link)

	// HELLO consumes 1 token; 4 more packets exhaust the bucket of 5.
	s.OnInbound(encode(t, clientEnvelope(rrc.THello, id, "", nil)))
	join(t, s, id, "lobby", "")
	for i := 0; i < 3; i++ {
		s.OnInbound(encode(t, clientEnvelope(rrc.TMsg, id, "lobby", "x")))
	}
	// The 6th inbound packet must be rate limited.
	s.OnInbound(encode(t, clientEnvelope(rrc.TMsg, id, "lobby", "overflow")))
	if got := lastError(t, link); got != "rate limited" {
		t.Errorf("expected rate limited error, got %q", got)
	}
}

// --- klines -----------------------------------------------------------

func TestKlineDisconnect(t *testing.T) {
	h := quietHub()
	opID := bytes.Repeat([]byte{0x11}, 16)
	h.cfg.TrustedIdentities = []string{hex.EncodeToString(opID)}
	h.reloadTrust()

	op, opLink := connect(t, h, opID)
	victimID := bytes.Repeat([]byte{0x22}, 16)
	_, victimLink := connect(t, h, victimID)

	op.OnInbound(encode(t, clientEnvelope(rrc.TMsg, opID, "",
		"/kline add "+hex.EncodeToString(victimID))))

	if !victimLink.isClosed() {
		t.Error("klined peer's link must be closed")
	}
	if got := lastNotice(t, opLink); !strings.Contains(got, "kline added") {
		t.Errorf("kline add notice: got %q", got)
	}
	if got := lastError(t, victimLink); got != "banned" {
		t.Errorf("victim error: got %q want banned", got)
	}
}

func TestBannedPeerDisconnectedOnConnect(t *testing.T) {
	bannedID := bytes.Repeat([]byte{0x33}, 16)
	cfg := config.HubConfig{BannedIdentities: []string{hex.EncodeToString(bannedID)}}
	h := quietHubCfg(cfg)
	link := &fakeLink{id: bannedID}
	h.NewSession(link)
	if !link.isClosed() {
		t.Error("a banned peer must be disconnected on connect")
	}
	if got := lastError(t, link); got != "banned" {
		t.Errorf("banned peer error: got %q", got)
	}
}
