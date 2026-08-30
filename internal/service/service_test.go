package service

import (
	"bytes"
	"encoding/hex"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/thatSFguy/reticulum-go/lxmf"
	"github.com/thatSFguy/reticulum-go/rns"
	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/hub"
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

// --- link-closed reaping (reticulum-go v0.7.0 §6.7.3) -----------------

// closeTestService builds the Service that onLinkClosed touches: a real
// hub (so sessionFor can make a Session) and a real Transport (so
// Session.Close can reach LinkManager through rnsLink).
func closeTestService(t *testing.T) *Service {
	t.Helper()
	quiet := log.New(io.Discard, "", 0)
	return &Service{
		log:        quiet,
		sessions:   make(map[string]*hub.Session),
		identities: make(map[string]peerBinding),
		hub:        hub.New(bytes.Repeat([]byte{0xFF}, rns.IdentityHashLen), config.HubConfig{}, quiet),
		transport:  rns.NewTransport(quiet),
	}
}

func sessionCount(s *Service) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// The 30-second janitor poll was not cosmetic: for that whole window
// the hub answered "is this person here?" wrongly in the direction that
// loses messages.
func TestAnObservedLinkCloseReapsTheSessionImmediately(t *testing.T) {
	svc := closeTestService(t)
	linkID := bytes.Repeat([]byte{0x5A}, rns.IdentityHashLen)

	if svc.sessionFor(linkID) == nil {
		t.Fatal("no session created")
	}
	if got := sessionCount(svc); got != 1 {
		t.Fatalf("session count = %d, want 1", got)
	}

	svc.onLinkClosed(linkID, rns.TeardownInitiatorClosed)

	if got := sessionCount(svc); got != 0 {
		t.Errorf("session count = %d after an observed close, want 0", got)
	}
}

// A local close is our own doing and the teardown that decided on it is
// already unwinding — rnsLink.Close calls CloseLink, which is what
// fires this callback. Acting again would re-enter state the first pass
// has not finished with.
func TestALocalCloseDoesNotReenterTeardown(t *testing.T) {
	svc := closeTestService(t)
	linkID := bytes.Repeat([]byte{0x5B}, rns.IdentityHashLen)
	svc.sessionFor(linkID)

	svc.onLinkClosed(linkID, rns.TeardownLocalClosed)

	if got := sessionCount(svc); got != 1 {
		t.Errorf("session count = %d; a local close reaped the session from under its own teardown", got)
	}
}

// The callback fires from more than one path and a retransmitted
// LINKCLOSE is normal, so a second delivery must be inert.
func TestReapingIsIdempotent(t *testing.T) {
	svc := closeTestService(t)
	linkID := bytes.Repeat([]byte{0x5C}, rns.IdentityHashLen)
	svc.sessionFor(linkID)

	svc.onLinkClosed(linkID, rns.TeardownInitiatorClosed)
	svc.onLinkClosed(linkID, rns.TeardownInitiatorClosed)
	svc.onLinkClosed(linkID, rns.TeardownTimeout)

	if got := sessionCount(svc); got != 0 {
		t.Errorf("session count = %d, want 0", got)
	}
}

// A close for a link this service never had a session for is normal on
// a hub whose transport also carries LXMF links.
func TestAClosedLinkWeNeverSawIsIgnored(t *testing.T) {
	svc := closeTestService(t)
	svc.onLinkClosed(bytes.Repeat([]byte{0x5D}, rns.IdentityHashLen), rns.TeardownTimeout)
	if got := sessionCount(svc); got != 0 {
		t.Errorf("session count = %d, want 0", got)
	}
}

// --- the notifications-only address -----------------------------------

// The hub must announce lxmf.delivery or nothing can verify its
// notifications (CLAUDE.md §5). The cost is that every messaging client
// lists it as a contact, so somebody will message it. Before this it
// went nowhere and they got silence, which looks exactly like a broken
// hub.
func TestInboundIsThrottledPerSender(t *testing.T) {
	svc := closeTestService(t)
	svc.lastInboundReply = map[string]time.Time{}
	src := bytes.Repeat([]byte{0x7A}, rns.IdentityHashLen)

	// No notifier installed, so nothing is sent — what is under test is
	// the throttle bookkeeping, which runs first.
	svc.answerInboundLXMF(&lxmf.Message{SourceHash: src})
	first := len(svc.lastInboundReply)
	svc.answerInboundLXMF(&lxmf.Message{SourceHash: src})

	if first != 1 {
		t.Fatalf("after one inbound the throttle holds %d entries, want 1", first)
	}
	if got := len(svc.lastInboundReply); got != 1 {
		t.Errorf("a repeat from the same sender added an entry (%d); the reply is itself "+
			"an LXMF send, so an unthrottled one is a round trip somebody else drives", got)
	}
}

// The throttle map is the one piece of per-sender state with no other
// bound, so it must forget what can no longer suppress anything.
func TestInboundThrottleIsSwept(t *testing.T) {
	svc := closeTestService(t)
	stale := bytes.Repeat([]byte{0x01}, rns.IdentityHashLen)
	svc.lastInboundReply = map[string]time.Time{
		hex.EncodeToString(stale): time.Now().Add(-2 * inboundReplyInterval),
	}

	svc.answerInboundLXMF(&lxmf.Message{SourceHash: bytes.Repeat([]byte{0x02}, rns.IdentityHashLen)})

	if _, ok := svc.lastInboundReply[hex.EncodeToString(stale)]; ok {
		t.Error("an expired throttle entry survived the sweep")
	}
}

func TestInboundWithNoSenderIsIgnored(t *testing.T) {
	svc := closeTestService(t)
	svc.answerInboundLXMF(nil)
	svc.answerInboundLXMF(&lxmf.Message{})
	if len(svc.lastInboundReply) != 0 {
		t.Error("a message with no source hash was recorded")
	}
}

// Attacker-supplied text reaches the log, so it must not be able to
// forge extra lines or run unbounded.
func TestTheLoggedSnippetIsBoundedAndSingleLine(t *testing.T) {
	got := snippetOf([]byte("title\nwith newlines"), []byte(bytes.NewBufferString("x").String()+
		string(bytes.Repeat([]byte("y"), 500))))
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("snippet carries newlines and could forge log lines: %q", got)
	}
	if len([]rune(got)) > 64 {
		t.Errorf("snippet is %d runes, want it bounded", len([]rune(got)))
	}
}
