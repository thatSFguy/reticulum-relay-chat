package hub

import (
	"bytes"
	"testing"
	"time"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// --- A9: shutdown race ------------------------------------------------

func TestClosedHubDropsInboundFrames(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0x0A}, 16)
	s, _ := connect(t, h, id)

	h.Stop() // marks the hub closed

	s.OnInbound(encode(t, clientEnvelope(rrc.TJoin, id, "lobby", nil)))
	if h.RoomCount() != 0 {
		t.Error("a closed hub must drop inbound frames instead of mutating state (audit A9)")
	}
}

// --- A6: resource send must not block the dispatch goroutine ----------

func TestResourceSendIsAsyncWithFallback(t *testing.T) {
	h := quietHubCfg(config.HubConfig{EnableResourceTransfer: true})
	id := bytes.Repeat([]byte{0x0B}, 16)
	link := &fakeLink{id: id, noResources: true} // SendResource will fail
	s := h.NewSession(link)
	s.OnInbound(encode(t, clientEnvelope(rrc.THello, id, "", nil))) // welcomed

	payload := []byte("a notice payload")
	if !s.tryResourceSend(payload, rrc.ResKindNotice, nil) {
		t.Fatal("tryResourceSend should return true when resource transfer is enabled")
	}
	// The RESOURCE_ENVELOPE is sent synchronously, before the goroutine.
	if _, ok := lastTypeBody(t, link, rrc.TResourceEnvelope); !ok {
		t.Error("RESOURCE_ENVELOPE must be sent synchronously")
	}
	// The transfer fails on the background goroutine, which then falls
	// back to a chunked NOTICE — it arrives shortly after, not inline.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if receivedNotice(link, string(payload)) {
			return // async fallback delivered
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("a failed resource send must fall back to a chunked NOTICE")
}
