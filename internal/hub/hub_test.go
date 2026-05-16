package hub

import (
	"bytes"
	"io"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// fakeLink is an in-memory hub.Link that records every frame sent to it.
type fakeLink struct {
	id   []byte
	mu   sync.Mutex
	sent [][]byte
}

func (f *fakeLink) Send(frame []byte) error {
	f.mu.Lock()
	f.sent = append(f.sent, append([]byte(nil), frame...))
	f.mu.Unlock()
	return nil
}
func (f *fakeLink) Close()                  {}
func (f *fakeLink) PeerIdentityHash() []byte { return f.id }

func (f *fakeLink) frames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.sent))
	copy(out, f.sent)
	return out
}

func quietHub() *Hub {
	hubID := bytes.Repeat([]byte{0xFF}, 16)
	return New(hubID, Config{Name: "Test Hub", Version: "test"},
		log.New(io.Discard, "", 0))
}

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

// TestMessageFanout drives two sessions through HELLO + JOIN and asserts
// a MSG from one is relayed to the other with K_SRC rewritten to the
// sender's link-verified identity.
func TestMessageFanout(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	linkA := &fakeLink{id: idA}
	linkB := &fakeLink{id: idB}
	sa := h.NewSession(linkA)
	sb := h.NewSession(linkB)

	for _, s := range []*Session{sa, sb} {
		src := idA
		if s == sb {
			src = idB
		}
		s.OnInbound(encode(t, clientEnvelope(rrc.THello, src, "", nil)))
		s.OnInbound(encode(t, clientEnvelope(rrc.TJoin, src, "lobby", nil)))
	}

	if h.RoomCount() != 1 {
		t.Fatalf("expected 1 room, got %d", h.RoomCount())
	}

	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "lobby", "hi there")))

	// linkB's last frame must be the relayed MSG.
	bFrames := linkB.frames()
	if len(bFrames) == 0 {
		t.Fatal("link B received no frames")
	}
	relayed, err := rrc.Decode(bFrames[len(bFrames)-1])
	if err != nil {
		t.Fatalf("decode relayed frame: %v", err)
	}
	if relayed.Type != rrc.TMsg {
		t.Fatalf("relayed type: got %d want %d", relayed.Type, rrc.TMsg)
	}
	if rrc.BodyText(relayed) != "hi there" {
		t.Errorf("relayed body: got %q", rrc.BodyText(relayed))
	}
	if !bytes.Equal(relayed.Src, idA) {
		t.Errorf("relayed K_SRC: got %x want %x (sender identity)", relayed.Src, idA)
	}
}

// TestJoinRequiresHello rejects a JOIN that arrives before HELLO with an
// ERROR rather than silently creating room state.
func TestJoinRequiresHello(t *testing.T) {
	h := quietHub()
	link := &fakeLink{id: bytes.Repeat([]byte{0x0C}, 16)}
	s := h.NewSession(link)

	s.OnInbound(encode(t, clientEnvelope(rrc.TJoin, link.id, "lobby", nil)))

	if h.RoomCount() != 0 {
		t.Errorf("a pre-HELLO JOIN must not create a room (got %d)", h.RoomCount())
	}
	frames := link.frames()
	if len(frames) != 1 {
		t.Fatalf("expected one ERROR frame, got %d", len(frames))
	}
	got, err := rrc.Decode(frames[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != rrc.TError {
		t.Errorf("expected ERROR, got type %d", got.Type)
	}
}

// TestGreetingAdvertisesRooms checks that a client's HELLO is answered
// with a NOTICE listing the hub's existing rooms — RRC has no room-list
// message, so this advert is how a fresh client discovers room names.
func TestGreetingAdvertisesRooms(t *testing.T) {
	h := quietHub()

	// First client creates a room.
	linkA := &fakeLink{id: bytes.Repeat([]byte{0xA1}, 16)}
	sa := h.NewSession(linkA)
	sa.OnInbound(encode(t, clientEnvelope(rrc.THello, linkA.id, "", nil)))
	sa.OnInbound(encode(t, clientEnvelope(rrc.TJoin, linkA.id, "alpha", nil)))

	// A second client connecting fresh must be told #alpha exists.
	linkB := &fakeLink{id: bytes.Repeat([]byte{0xB2}, 16)}
	sb := h.NewSession(linkB)
	sb.OnInbound(encode(t, clientEnvelope(rrc.THello, linkB.id, "", nil)))

	advertised := false
	for _, f := range linkB.frames() {
		env, err := rrc.Decode(f)
		if err != nil {
			continue
		}
		if env.Type == rrc.TNotice && strings.Contains(rrc.BodyText(env), "#alpha") {
			advertised = true
		}
	}
	if !advertised {
		t.Error("second client's HELLO got no room-directory NOTICE listing #alpha")
	}
}

// TestPingPong checks the hub answers a client PING with a PONG that
// echoes the payload.
func TestPingPong(t *testing.T) {
	h := quietHub()
	link := &fakeLink{id: bytes.Repeat([]byte{0x0D}, 16)}
	s := h.NewSession(link)

	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	s.OnInbound(encode(t, clientEnvelope(rrc.TPing, link.id, "", payload)))

	frames := link.frames()
	if len(frames) != 1 {
		t.Fatalf("expected one PONG frame, got %d", len(frames))
	}
	pong, err := rrc.Decode(frames[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pong.Type != rrc.TPong {
		t.Fatalf("expected PONG, got type %d", pong.Type)
	}
	if !bytes.Equal(rrc.BodyBytes(pong), payload) {
		t.Errorf("PONG payload: got %x want %x", rrc.BodyBytes(pong), payload)
	}
}
