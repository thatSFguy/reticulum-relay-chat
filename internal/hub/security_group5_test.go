package hub

import (
	"bytes"
	"encoding/hex"
	"log"
	"strings"
	"testing"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/peerreg"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// Group 5: the audit of 2026-08-31.
//
// Every test here failed before the fix beside it. They are grouped
// because they share one shape — a check that existed in one place and
// was missing from its sibling — which is the shape worth looking for
// next time.

// --- A21: /topic set had no membership or ban check -------------------

// Commands are dispatched BEFORE any room gate (handleMsg), so a peer
// banned from a room still reached cmdTopic. Only +t was checked, and a
// non-+t room could be retitled by anybody who knew its name — persisted
// to rooms.toml when registered, and broadcast to every member.
func TestBannedNonMemberCannotSetTopic(t *testing.T) {
	h := quietHub()
	alice := bytes.Repeat([]byte{0xA1}, 16)
	mallory := bytes.Repeat([]byte{0xB2}, 16)

	sa, _ := connect(t, h, alice)
	join(t, sa, alice, "lobby", "")
	sm, lm := connect(t, h, mallory)

	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, alice, "lobby",
		"/ban lobby add "+hex.EncodeToString(mallory))))
	h.mu.Lock()
	_, banned := h.roomLocked("lobby").bans[hex.EncodeToString(mallory)]
	h.mu.Unlock()
	if !banned {
		t.Fatal("setup: mallory was not banned")
	}

	sm.OnInbound(encode(t, clientEnvelope(rrc.TMsg, mallory, "",
		"/topic lobby OWNED BY MALLORY")))

	h.mu.Lock()
	got := h.roomLocked("lobby").topic
	h.mu.Unlock()
	if got == "OWNED BY MALLORY" {
		t.Error("a banned non-member set the room topic")
	}
	if e := lastError(t, lm); !strings.Contains(e, "join the room") {
		t.Errorf("mallory was told %q, want the membership refusal", e)
	}
}

// A member sets the topic normally — the fix must not lock out the
// people the room belongs to.
func TestAMemberCanStillSetTheTopic(t *testing.T) {
	h := quietHub()
	alice := bytes.Repeat([]byte{0xA1}, 16)
	sa, _ := connect(t, h, alice)
	join(t, sa, alice, "lobby", "")

	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, alice, "lobby", "/topic lobby fish")))
	h.mu.Lock()
	got := h.roomLocked("lobby").topic
	h.mu.Unlock()
	if got != "fish" {
		t.Errorf("topic = %q, want %q", got, "fish")
	}
}

// A server-op is not required to join a room to administer it, which is
// how every other room command here already treats one.
func TestAServerOpCanSetTheTopicWithoutJoining(t *testing.T) {
	op := bytes.Repeat([]byte{0x0B}, 16)
	h := quietHubCfg(config.HubConfig{
		TrustedIdentities: []string{hex.EncodeToString(op)},
	})
	alice := bytes.Repeat([]byte{0xA1}, 16)
	sa, _ := connect(t, h, alice)
	join(t, sa, alice, "lobby", "")

	so, _ := connect(t, h, op)
	so.OnInbound(encode(t, clientEnvelope(rrc.TMsg, op, "", "/topic lobby by the op")))

	h.mu.Lock()
	got := h.roomLocked("lobby").topic
	h.mu.Unlock()
	if got != "by the op" {
		t.Errorf("topic = %q, want the server-op's", got)
	}
}

// --- A22: a command body escaped max_msg_body_bytes -------------------

// handleMsg dispatches a slash command before the body-size check, so
// the limit never applied to one. /topic then wrote its unbounded
// argument to rooms.toml and re-sent it to every joiner.
func TestACommandBodyIsBoundedLikeAMessage(t *testing.T) {
	cfg := config.HubConfig{Limits: config.LimitsConfig{
		MaxNickBytes: 32, MaxRoomNameBytes: 64,
		MaxMsgBodyBytes: 350, MaxRoomsPerSession: 16, RateLimitMsgsPerMin: 240,
	}}
	h := quietHubCfg(cfg)
	alice := bytes.Repeat([]byte{0xA1}, 16)
	sa, la := connect(t, h, alice)
	join(t, sa, alice, "lobby", "")

	// The ordinary-message limit, for contrast.
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, alice, "lobby", strings.Repeat("x", 400))))
	if e := lastError(t, la); !strings.Contains(e, "body-size limit") {
		t.Fatalf("setup: a 400-byte message body was not refused; got %q", e)
	}

	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, alice, "lobby",
		"/topic lobby "+strings.Repeat("A", 7000))))
	if e := lastError(t, la); !strings.Contains(e, "command exceeds") {
		t.Errorf("a 7000-byte command was answered %q, want the body-size refusal", e)
	}
	h.mu.Lock()
	got := h.roomLocked("lobby").topic
	h.mu.Unlock()
	if got != "" {
		t.Errorf("stored a %d-byte topic from an over-limit command", len(got))
	}
}

// The topic carries its own bound too: it is the field that reaches disk
// and every future joiner, so it must not inherit whatever
// max_msg_body_bytes happens to be on a hub that raised it.
func TestTopicHasItsOwnBoundAboveTheMessageLimit(t *testing.T) {
	cfg := config.HubConfig{
		MaxTopicBytes: 40,
		Limits: config.LimitsConfig{
			MaxNickBytes: 32, MaxRoomNameBytes: 64,
			MaxMsgBodyBytes: 4096, MaxRoomsPerSession: 16, RateLimitMsgsPerMin: 240,
		},
	}
	h := quietHubCfg(cfg)
	alice := bytes.Repeat([]byte{0xA1}, 16)
	sa, la := connect(t, h, alice)
	join(t, sa, alice, "lobby", "")

	// Well inside max_msg_body_bytes, well outside max_topic_bytes.
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, alice, "lobby",
		"/topic lobby "+strings.Repeat("A", 200))))
	if e := lastError(t, la); !strings.Contains(e, "topic exceeds") {
		t.Errorf("got %q, want the topic-length refusal", e)
	}
	h.mu.Lock()
	got := h.roomLocked("lobby").topic
	h.mu.Unlock()
	if got != "" {
		t.Errorf("stored a %d-byte topic over a %d-byte limit", len(got), cfg.MaxTopicBytes)
	}
}

// The two bounds must fit each other. A topic arrives INSIDE the command
// that sets it, so a max_topic_bytes equal to max_msg_body_bytes would
// advertise a length no client could ever send — the top of the range
// unreachable, and the refusal blaming the wrong limit.
//
// Asserted against the SHIPPED defaults, because that is the pairing
// every untuned hub runs.
func TestAMaximumLengthTopicFitsInsideTheCommandThatSetsIt(t *testing.T) {
	d := config.DefaultsForTest()
	worstCaseRoom := strings.Repeat("r", d.Limits.MaxRoomNameBytes)
	line := "/topic " + worstCaseRoom + " " + strings.Repeat("t", d.MaxTopicBytes)
	if len(line) > d.Limits.MaxMsgBodyBytes {
		t.Errorf("a maximum topic (%d) in a maximum room name (%d) makes a %d-byte command, over the %d-byte body limit — the last %d bytes of max_topic_bytes are unreachable",
			d.MaxTopicBytes, d.Limits.MaxRoomNameBytes, len(line),
			d.Limits.MaxMsgBodyBytes, len(line)-d.Limits.MaxMsgBodyBytes)
	}
}

// --- A23: eviction destroyed the mentions it should protect -----------

// A peer with queued mentions is by definition one not seen lately, so
// oldest-last-seen eviction selected exactly the rows the queue exists
// to hold. Ordinary churn was enough to flush every held mention.
func TestEvictionSpareThePeerWhoIsOwedMentions(t *testing.T) {
	h := quietHubCfg(config.HubConfig{MentionNotify: true, MaxKnownPeers: 3})
	victim := strings.Repeat("11", 16)

	h.mu.Lock()
	h.peers[victim] = &peerreg.Peer{IdentityHex: victim, Nick: "victim", LastSeenTS: 1}
	h.queueMentionLocked(victim, peerreg.Mention{Room: "lobby", ByNick: "alice", Text: "@victim ping"})
	h.mu.Unlock()

	// Strangers arrive later — every one of them more recently seen.
	for i := 0; i < 5; i++ {
		id := strings.Repeat(hex.EncodeToString([]byte{byte(0x20 + i)}), 16)
		h.mu.Lock()
		if len(h.peers) >= h.cfg.MaxKnownPeers {
			h.evictOldestPeerLocked()
		}
		h.peers[id] = &peerreg.Peer{IdentityHex: id, LastSeenTS: float64(100 + i)}
		h.mu.Unlock()
	}

	h.mu.Lock()
	p, still := h.peers[victim]
	h.mu.Unlock()
	if !still {
		t.Fatal("the peer owed a mention was evicted by stranger churn")
	}
	if len(p.Mentions) != 1 {
		t.Errorf("victim holds %d mentions, want 1", len(p.Mentions))
	}
}

// The ordinary case must still evict: sparing peers with mail is not a
// licence to grow the directory past its cap.
func TestEvictionStillDropsTheOldestPeerWithNothingPending(t *testing.T) {
	h := quietHubCfg(config.HubConfig{MentionNotify: true, MaxKnownPeers: 2})
	old := strings.Repeat("aa", 16)
	recent := strings.Repeat("bb", 16)

	h.mu.Lock()
	h.peers[old] = &peerreg.Peer{IdentityHex: old, LastSeenTS: 1}
	h.peers[recent] = &peerreg.Peer{IdentityHex: recent, LastSeenTS: 99}
	h.evictOldestPeerLocked()
	_, oldStill := h.peers[old]
	_, recentStill := h.peers[recent]
	h.mu.Unlock()

	if oldStill {
		t.Error("the oldest peer with nothing pending was not evicted")
	}
	if !recentStill {
		t.Error("the recently seen peer was evicted instead")
	}
}

// When everyone is owed something, one still has to go — silently doing
// nothing would stop the directory accepting anybody new.
func TestEvictionFallsBackWhenEveryPeerIsOwed(t *testing.T) {
	h := quietHubCfg(config.HubConfig{MentionNotify: true, MaxKnownPeers: 2})
	old := strings.Repeat("aa", 16)
	recent := strings.Repeat("bb", 16)

	h.mu.Lock()
	h.peers[old] = &peerreg.Peer{IdentityHex: old, LastSeenTS: 1}
	h.peers[recent] = &peerreg.Peer{IdentityHex: recent, LastSeenTS: 99}
	h.queueMentionLocked(old, peerreg.Mention{Room: "lobby", Text: "a"})
	h.queueMentionLocked(recent, peerreg.Mention{Room: "lobby", Text: "b"})
	h.evictOldestPeerLocked()
	n := len(h.peers)
	_, oldStill := h.peers[old]
	h.mu.Unlock()

	if n != 1 {
		t.Errorf("directory holds %d peers, want 1 — nothing was evicted", n)
	}
	if oldStill {
		t.Error("evicted the newer peer; the oldest should fall when all are owed")
	}
}

// --- A24: a member could not /who their own +p room -------------------

func TestAMemberCanWhoTheirOwnPrivateRoom(t *testing.T) {
	h := quietHub()
	alice := bytes.Repeat([]byte{0xA1}, 16)
	sa, la := connect(t, h, alice)
	join(t, sa, alice, "lobby", "")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, alice, "lobby", "/mode lobby +p")))

	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, alice, "lobby", "/who lobby")))
	got := lastNotice(t, la)
	if strings.Contains(got, "is private") {
		t.Errorf("a member of +p lobby was refused /who: %q", got)
	}
	if !strings.Contains(got, "members in lobby") {
		t.Errorf("/who answered %q, want the member list", got)
	}
}

// +p must still hide the room from outside, which is what it is for.
func TestANonMemberStillCannotWhoAPrivateRoom(t *testing.T) {
	h := quietHub()
	alice := bytes.Repeat([]byte{0xA1}, 16)
	mallory := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, alice)
	join(t, sa, alice, "lobby", "")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, alice, "lobby", "/mode lobby +p")))

	sm, lm := connect(t, h, mallory)
	sm.OnInbound(encode(t, clientEnvelope(rrc.TMsg, mallory, "", "/who lobby")))
	if got := lastNotice(t, lm); !strings.Contains(got, "is private") {
		t.Errorf("a non-member saw %q, want the private refusal", got)
	}
}

// --- A25: configured identity hashes were never validated -------------

// isServerOp compares against the hex of a full 16-byte identity hash,
// so an entry of any other shape can never fire. Storing it anyway left
// the operator with a permanent, unexplained "not authorized".
func TestAMalformedTrustedIdentityIsRejectedAndLogged(t *testing.T) {
	var logbuf strings.Builder
	good := strings.Repeat("cc", 16)
	cfg := config.HubConfig{
		Name: "T", Version: "t",
		TrustedIdentities: []string{
			good,
			"nothex!!",                               // not hex at all
			strings.Repeat("dd", 8),                  // a plausible half-length hash
			strings.Repeat("ee", 32),                 // a destination-hash-shaped 32 bytes
			"  0X" + strings.Repeat("ff", 16) + "  ", // valid, oddly spelled
		},
		BannedIdentities: []string{"zzzz"},
		Limits: config.LimitsConfig{
			MaxNickBytes: 32, MaxRoomNameBytes: 64, MaxMsgBodyBytes: 350,
			MaxRoomsPerSession: 16, RateLimitMsgsPerMin: 240,
		},
	}
	h := New(bytes.Repeat([]byte{0xFF}, 16), cfg, log.New(&logbuf, "", 0))

	h.mu.Lock()
	_, hasGood := h.trusted[good]
	_, hasOdd := h.trusted[strings.Repeat("ff", 16)]
	n := len(h.trusted)
	nBanned := len(h.banned)
	h.mu.Unlock()

	if !hasGood || !hasOdd {
		t.Errorf("a well-formed entry was dropped: trusted=%v", h.trusted)
	}
	if n != 2 {
		t.Errorf("trusted holds %d entries, want 2 — a malformed one was stored", n)
	}
	if nBanned != 0 {
		t.Errorf("banned holds %d entries, want 0", nBanned)
	}
	out := logbuf.String()
	for _, want := range []string{"trusted_identities", "banned_identities"} {
		if !strings.Contains(out, want) {
			t.Errorf("nothing in the log names %s; an operator gets no evidence:\n%s", want, out)
		}
	}
}

// A hash that IS well-formed still authorizes — the validation must not
// cost a real server-op their access.
func TestAValidTrustedIdentityStillAuthorizes(t *testing.T) {
	op := bytes.Repeat([]byte{0x0B}, 16)
	h := quietHubCfg(config.HubConfig{TrustedIdentities: []string{hex.EncodeToString(op)}})
	s, link := connect(t, h, op)
	s.OnInbound(encode(t, clientEnvelope(rrc.TMsg, op, "", "/stats")))
	if e := lastError(t, link); strings.Contains(e, "not authorized") {
		t.Errorf("a valid server-op was refused /stats: %q", e)
	}
}
