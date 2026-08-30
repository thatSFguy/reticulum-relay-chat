package hub

import (
	"bytes"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// Extension keys (docs/rrc-extensions.md).
//
// The hub REBUILDS every envelope it forwards, because it must rewrite
// K_SRC to the link-verified identity — that is what stops a client
// speaking as somebody else. So "relay it verbatim" is not the absence
// of work here, it is work: anything the struct has no field for is
// dropped on that rebuild, silently, and a client feature built on it
// fails with no error anywhere.

// sendRaw feeds a hand-built envelope map, so a test can put keys on
// the wire that rrc.Envelope would not otherwise produce.
func sendRaw(t *testing.T, s *Session, m map[int]any) {
	t.Helper()
	frame, err := cbor.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s.OnInbound(frame)
}

// relayedKeys returns the key set of the last MSG a link received.
func relayedKeys(t *testing.T, link *fakeLink) map[uint64]any {
	t.Helper()
	frames := link.frames()
	for i := len(frames) - 1; i >= 0; i-- {
		var m map[any]any
		if cbor.Unmarshal(frames[i], &m) != nil {
			continue
		}
		out := map[uint64]any{}
		for k, v := range m {
			switch u := k.(type) {
			case uint64:
				out[u] = v
			case int64:
				if u >= 0 {
					out[uint64(u)] = v
				}
			}
		}
		if t, ok := out[uint64(rrc.KT)]; ok {
			if n, ok := t.(uint64); ok && n == uint64(rrc.TMsg) {
				return out
			}
		}
	}
	return nil
}

func msgWithExt(id []byte, room, body string, ext map[int]any) map[int]any {
	m := map[int]any{
		rrc.KV: 1, rrc.KT: rrc.TMsg, rrc.KID: []byte("abcdefgh"),
		rrc.KTS: int64(1), rrc.KSrc: id, rrc.KRoom: room, rrc.KBody: body,
	}
	for k, v := range ext {
		m[k] = v
	}
	return m
}

func twoInARoom(t *testing.T, h *Hub) (*Session, []byte, *fakeLink) {
	t.Helper()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)
	join(t, sa, idA, "lobby", "")
	join(t, sb, idB, "lobby", "")
	return sa, idA, linkB
}

// --- the contract -------------------------------------------------------

func TestExtensionKeysAreRelayedVerbatim(t *testing.T) {
	h := quietHub()
	sa, idA, linkB := twoInARoom(t, h)
	target := []byte("a41b9c33")

	sendRaw(t, sa, msgWithExt(idA, "lobby", "yes, exactly that",
		map[int]any{rrc.KReplyTo: target}))

	got := relayedKeys(t, linkB)
	if got == nil {
		t.Fatal("no MSG was relayed")
	}
	v, ok := got[rrc.KReplyTo]
	if !ok {
		t.Fatal("K_REPLY_TO was dropped on fan-out; a reply arrives as an ordinary message")
	}
	if b, _ := v.([]byte); !bytes.Equal(b, target) {
		t.Errorf("K_REPLY_TO = %x, want %x", b, target)
	}
}

func TestEveryDefinedExtensionKeySurvives(t *testing.T) {
	h := quietHub()
	sa, idA, linkB := twoInARoom(t, h)

	sendRaw(t, sa, msgWithExt(idA, "lobby", "👍", map[int]any{
		rrc.KReactTo: []byte("a41b9c33"),
		rrc.KReactOp: uint64(1),
	}))

	got := relayedKeys(t, linkB)
	for _, k := range []int{rrc.KReactTo, rrc.KReactOp} {
		if _, ok := got[uint64(k)]; !ok {
			t.Errorf("key %d was dropped", k)
		}
	}
	if body, _ := got[uint64(rrc.KBody)].(string); body != "👍" {
		t.Errorf("reaction body = %q, want the emoji", body)
	}
}

// Keys 8..63 belong to a future RRC core. A hub that passed them
// through would let a client pre-empt a key the spec has not assigned.
func TestReservedKeysAreStillDropped(t *testing.T) {
	h := quietHub()
	sa, idA, linkB := twoInARoom(t, h)

	sendRaw(t, sa, msgWithExt(idA, "lobby", "hi", map[int]any{
		9:  "squatting on a future core key",
		63: "likewise",
	}))

	got := relayedKeys(t, linkB)
	for _, k := range []uint64{9, 63} {
		if _, ok := got[k]; ok {
			t.Errorf("reserved key %d was relayed; it belongs to a future RRC core", k)
		}
	}
}

// K_SRC is rewritten to the link-verified identity, and that is what
// stops a client speaking as somebody else. Nothing in the extension
// range may undo it.
func TestAnExtensionCannotOverwriteAVerifiedCoreKey(t *testing.T) {
	h := quietHub()
	sa, idA, linkB := twoInARoom(t, h)
	impostor := bytes.Repeat([]byte{0xEE}, 16)

	// Claim a different sender outright, and also smuggle one in the
	// extension range.
	m := msgWithExt(idA, "lobby", "not who I say I am",
		map[int]any{rrc.KReplyTo: []byte("a41b9c33")})
	m[rrc.KSrc] = impostor
	sendRaw(t, sa, m)

	got := relayedKeys(t, linkB)
	src, _ := got[uint64(rrc.KSrc)].([]byte)
	if bytes.Equal(src, impostor) {
		t.Fatal("the hub relayed a client-claimed K_SRC")
	}
	if !bytes.Equal(src, idA) {
		t.Errorf("K_SRC = %x, want the link-verified %x", src, idA)
	}
}

// §5: a hub is a fan-out amplifier — one inbound frame becomes one per
// member — so unread relayed bytes are multiplied by the room size.
// Rejected, not truncated: half a reply reference threads wrongly while
// looking fine.
func TestOversizedExtensionsAreRejectedNotTruncated(t *testing.T) {
	h := quietHub()
	sa, idA, linkA := connectInRoom(t, h)
	_ = linkA

	sendRaw(t, sa, msgWithExt(idA, "lobby", "hi",
		map[int]any{rrc.KReplyTo: bytes.Repeat([]byte{0x41}, maxExtBytes+64)}))

	if got := lastError(t, linkA); got == "" {
		t.Fatal("an oversized extension payload was accepted silently")
	} else if !bytes.Contains([]byte(got), []byte("extension keys exceed")) {
		t.Errorf("error = %q, want it to name the extension limit", got)
	}
}

func connectInRoom(t *testing.T, h *Hub) (*Session, []byte, *fakeLink) {
	t.Helper()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	join(t, s, id, "lobby", "")
	return s, id, link
}

// A room opens with its replay. If that loses threading, the feature
// works live and breaks on rejoin — the harder failure to notice.
func TestAReplayedReplyStillThreads(t *testing.T) {
	h := historyHub(t, nil)
	sa, idA, _ := twoInARoom(t, h)
	target := []byte("a41b9c33")

	sendRaw(t, sa, msgWithExt(idA, "lobby", "yes, exactly that",
		map[int]any{rrc.KReplyTo: target}))

	// A third peer joins and gets the replay.
	idC := bytes.Repeat([]byte{0xC3}, 16)
	sc, linkC := connect(t, h, idC)
	join(t, sc, idC, "lobby", "")

	got := relayedKeys(t, linkC)
	if got == nil {
		t.Fatal("nothing was replayed")
	}
	v, ok := got[rrc.KReplyTo]
	if !ok {
		t.Fatal("the replayed reply lost K_REPLY_TO")
	}
	if b, _ := v.([]byte); !bytes.Equal(b, target) {
		t.Errorf("replayed K_REPLY_TO = %x, want %x", b, target)
	}
}
