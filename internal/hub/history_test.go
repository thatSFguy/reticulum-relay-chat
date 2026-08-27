package hub

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// historyHub builds a hub retaining transcripts under a temp directory.
func historyHub(t *testing.T, tune func(*config.HubConfig)) *Hub {
	t.Helper()
	dir := t.TempDir()
	cfg := config.HubConfig{
		// /register needs somewhere to persist, and these tests turn on
		// registration to get rooms that outlive their last member.
		RoomRegistryPath:       filepath.Join(dir, "rooms.toml"),
		HistoryEnabled:         true,
		HistoryPath:            filepath.Join(dir, "history"),
		HistoryRetention:       config.Duration{Duration: 7 * 24 * 3600e9},
		HistoryMaxBytesPerRoom: 1 << 20,
		HistoryMaxTotalBytes:   1 << 22,
		HistoryReplayCount:     10,
		HistoryReplayBytes:     4096,
		HistoryPullCount:       100,
		HistoryPullBytes:       16384,
	}
	if tune != nil {
		tune(&cfg)
	}
	h := quietHubCfg(cfg)
	if !h.historyEnabled() {
		t.Fatal("history did not open")
	}
	return h
}

// say sends one MSG from a session into a room.
func say(t *testing.T, s *Session, id []byte, room, text string) {
	t.Helper()
	s.OnInbound(encode(t, clientEnvelope(rrc.TMsg, id, room, text)))
}

// replayedBodies returns the MSG/ACTION bodies a link received, in
// order, ignoring the sender's own echo of nothing and any NOTICEs.
func replayedBodies(t *testing.T, link *fakeLink) []string {
	t.Helper()
	var out []string
	for _, f := range link.frames() {
		env, err := rrc.Decode(f)
		if err != nil {
			continue
		}
		if env.Type == rrc.TMsg || env.Type == rrc.TAction {
			out = append(out, rrc.BodyText(env))
		}
	}
	return out
}

func noticesOn(t *testing.T, link *fakeLink) []string {
	t.Helper()
	var out []string
	for _, f := range link.frames() {
		env, err := rrc.Decode(f)
		if err != nil {
			continue
		}
		if env.Type == rrc.TNotice {
			out = append(out, rrc.BodyText(env))
		}
	}
	return out
}

func hasPrefixIn(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// The headline behavior: someone who was not there when it was said
// still sees the conversation on joining.
func TestAJoinerIsReplayedWhatTheyMissed(t *testing.T) {
	h := historyHub(t, nil)
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)

	// The room must outlive A's visit, or its history goes with it —
	// so register it, which is what "this room is a place" means here.
	sa, _ := connect(t, h, idA)
	join(t, sa, idA, "#lobby", "")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "#lobby", "/register #lobby")))
	say(t, sa, idA, "#lobby", "first")
	say(t, sa, idA, "#lobby", "second")

	sb, linkB := connect(t, h, idB)
	join(t, sb, idB, "#lobby", "")

	got := replayedBodies(t, linkB)
	if want := []string{"first", "second"}; !equalStrings(got, want) {
		t.Errorf("joiner saw %v, want %v", got, want)
	}
}

// A replay must be marked as one. The brackets are ordinary NOTICEs, so
// an unmodified client shows them as text and loses nothing.
func TestAReplayIsBracketedByNotices(t *testing.T) {
	h := historyHub(t, nil)
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)

	sa, _ := connect(t, h, idA)
	join(t, sa, idA, "#lobby", "")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "#lobby", "/register #lobby")))
	say(t, sa, idA, "#lobby", "hello")

	sb, linkB := connect(t, h, idB)
	join(t, sb, idB, "#lobby", "")

	notices := noticesOn(t, linkB)
	if !hasPrefixIn(notices, "--- 1 message") {
		t.Errorf("no replay header among %v", notices)
	}
	if !hasPrefixIn(notices, "--- end of history ---") {
		t.Errorf("no replay footer among %v", notices)
	}
}

// A replayed message must be the original envelope, not a new one: the
// id is how a client recognises a message it already rendered, and the
// timestamp is when it was said.
func TestAReplayPreservesTheOriginalIdAndTimestamp(t *testing.T) {
	h := historyHub(t, nil)
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)

	sa, linkA := connect(t, h, idA)
	join(t, sa, idA, "#lobby", "")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "#lobby", "/register #lobby")))

	sent := clientEnvelope(rrc.TMsg, idA, "#lobby", "remember me")
	sent.TimestampMs = 1_700_000_000_123
	sa.OnInbound(encode(t, sent))

	relayed, ok := lastTypeBody(t, linkA, rrc.TMsg)
	if !ok {
		t.Fatal("the message was not relayed live")
	}

	sb, linkB := connect(t, h, idB)
	join(t, sb, idB, "#lobby", "")

	replayed, ok := lastTypeBody(t, linkB, rrc.TMsg)
	if !ok {
		t.Fatal("nothing was replayed")
	}
	if !bytes.Equal(replayed.MsgID, relayed.MsgID) {
		t.Errorf("replayed id %x, live id %x — a client cannot tell these are the same message",
			replayed.MsgID, relayed.MsgID)
	}
	if replayed.TimestampMs != sent.TimestampMs {
		t.Errorf("replayed timestamp %d, want the original %d", replayed.TimestampMs, sent.TimestampMs)
	}
	if !bytes.Equal(replayed.Src, idA) {
		t.Errorf("replayed K_SRC %x, want the verified sender %x", replayed.Src, idA)
	}
}

// What is retained is what the room saw, so a client that lies about
// its K_SRC does not get to write that lie into the transcript.
func TestTheTranscriptKeepsTheVerifiedSenderNotTheClaimedOne(t *testing.T) {
	h := historyHub(t, nil)
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	spoofed := bytes.Repeat([]byte{0xEE}, 16)

	sa, _ := connect(t, h, idA)
	join(t, sa, idA, "#lobby", "")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "#lobby", "/register #lobby")))
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, spoofed, "#lobby", "it was not me")))

	sb, linkB := connect(t, h, idB)
	join(t, sb, idB, "#lobby", "")

	replayed, ok := lastTypeBody(t, linkB, rrc.TMsg)
	if !ok {
		t.Fatal("nothing was replayed")
	}
	if bytes.Equal(replayed.Src, spoofed) {
		t.Error("the transcript kept a spoofed K_SRC")
	}
	if !bytes.Equal(replayed.Src, idA) {
		t.Errorf("replayed K_SRC %x, want the link-verified %x", replayed.Src, idA)
	}
}

// An unregistered room dies with its last member. Its transcript must
// not survive to greet whoever creates a room of that name next.
func TestAnEphemeralRoomsHistoryDoesNotOutliveIt(t *testing.T) {
	h := historyHub(t, nil)
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)

	sa, _ := connect(t, h, idA)
	join(t, sa, idA, "#temp", "")
	say(t, sa, idA, "#temp", "said in passing")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TPart, idA, "#temp", nil)))

	sb, linkB := connect(t, h, idB)
	join(t, sb, idB, "#temp", "")

	if got := replayedBodies(t, linkB); len(got) != 0 {
		t.Errorf("a recreated ephemeral room replayed %v", got)
	}
}

// A registered room is a place that persists, so its transcript should
// survive the room going empty.
func TestARegisteredRoomsHistorySurvivesGoingEmpty(t *testing.T) {
	h := historyHub(t, nil)
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)

	sa, _ := connect(t, h, idA)
	join(t, sa, idA, "#lobby", "")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "#lobby", "/register #lobby")))
	say(t, sa, idA, "#lobby", "still here tomorrow")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TPart, idA, "#lobby", nil)))

	sb, linkB := connect(t, h, idB)
	join(t, sb, idB, "#lobby", "")

	if got := replayedBodies(t, linkB); !equalStrings(got, []string{"still here tomorrow"}) {
		t.Errorf("registered room replayed %v, want the retained message", got)
	}
}

// The join replay is bounded so a slow link is not flooded; the rest is
// available on request rather than pushed.
func TestTheJoinReplayIsBoundedByTheConfiguredCount(t *testing.T) {
	h := historyHub(t, func(c *config.HubConfig) { c.HistoryReplayCount = 2 })
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)

	sa, _ := connect(t, h, idA)
	join(t, sa, idA, "#lobby", "")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "#lobby", "/register #lobby")))
	for _, text := range []string{"one", "two", "three", "four"} {
		say(t, sa, idA, "#lobby", text)
	}

	sb, linkB := connect(t, h, idB)
	join(t, sb, idB, "#lobby", "")

	if got := replayedBodies(t, linkB); !equalStrings(got, []string{"three", "four"}) {
		t.Errorf("join replay was %v, want the last two", got)
	}
}

func TestHistoryCommandServesMoreThanTheJoinReplay(t *testing.T) {
	h := historyHub(t, func(c *config.HubConfig) { c.HistoryReplayCount = 1 })
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)

	sa, _ := connect(t, h, idA)
	join(t, sa, idA, "#lobby", "")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "#lobby", "/register #lobby")))
	for _, text := range []string{"one", "two", "three"} {
		say(t, sa, idA, "#lobby", text)
	}

	sb, linkB := connect(t, h, idB)
	join(t, sb, idB, "#lobby", "")
	if got := replayedBodies(t, linkB); len(got) != 1 {
		t.Fatalf("join replay was %v, want one message", got)
	}

	sb.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idB, "#lobby", "/history #lobby 10")))

	got := replayedBodies(t, linkB)
	if len(got) < 4 {
		t.Errorf("after /history the link carried %v, want the join replay plus all three", got)
	}
}

// Reading a transcript is gated on being in the room: membership is
// where the key, invite and ban checks already happened.
func TestHistoryCommandRefusesANonMember(t *testing.T) {
	h := historyHub(t, nil)
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)

	sa, _ := connect(t, h, idA)
	join(t, sa, idA, "#private", "")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "#private", "/register #private")))
	say(t, sa, idA, "#private", "members only")

	sb, linkB := connect(t, h, idB)
	sb.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idB, "", "/history #private")))

	if got := replayedBodies(t, linkB); len(got) != 0 {
		t.Errorf("a non-member was served %v", got)
	}
	if !strings.Contains(lastError(t, linkB), "join the room") {
		t.Errorf("expected a refusal, got %q", lastError(t, linkB))
	}
}

func TestHistoryPurgeRequiresARoomOperator(t *testing.T) {
	h := historyHub(t, nil)
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)

	sa, _ := connect(t, h, idA) // founder, therefore op
	join(t, sa, idA, "#lobby", "")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "#lobby", "/register #lobby")))
	say(t, sa, idA, "#lobby", "delete me")

	sb, linkB := connect(t, h, idB)
	join(t, sb, idB, "#lobby", "")
	sb.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idB, "#lobby", "/history purge #lobby")))
	if !strings.Contains(lastError(t, linkB), "not authorized") {
		t.Errorf("a non-op purge was not refused: %q", lastError(t, linkB))
	}

	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "#lobby", "/history purge #lobby")))

	idC := bytes.Repeat([]byte{0xC3}, 16)
	sc, linkC := connect(t, h, idC)
	join(t, sc, idC, "#lobby", "")
	if got := replayedBodies(t, linkC); len(got) != 0 {
		t.Errorf("history survived a purge: %v", got)
	}
}

// A hub with history off must behave exactly as it did before: no
// replay, no store, and a clear answer to anyone who asks.
func TestHistoryOffChangesNothing(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)

	sa, _ := connect(t, h, idA)
	join(t, sa, idA, "#lobby", "")
	say(t, sa, idA, "#lobby", "into the void")

	sb, linkB := connect(t, h, idB)
	join(t, sb, idB, "#lobby", "")
	if got := replayedBodies(t, linkB); len(got) != 0 {
		t.Errorf("a hub with history disabled replayed %v", got)
	}

	sb.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idB, "#lobby", "/history")))
	if !strings.Contains(lastNotice(t, linkB), "does not retain history") {
		t.Errorf("expected a clear answer, got %q", lastNotice(t, linkB))
	}
}

// Slash commands are consumed by the hub, never relayed — so they must
// never be retained either, or a purge command would be replayed to the
// next joiner.
func TestCommandsAreNotRetained(t *testing.T) {
	h := historyHub(t, nil)
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)

	sa, _ := connect(t, h, idA)
	join(t, sa, idA, "#lobby", "")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "#lobby", "/register #lobby")))
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "#lobby", "/topic #lobby hello")))
	say(t, sa, idA, "#lobby", "real message")

	sb, linkB := connect(t, h, idB)
	join(t, sb, idB, "#lobby", "")

	for _, body := range replayedBodies(t, linkB) {
		if strings.HasPrefix(body, "/") {
			t.Errorf("a slash command was retained and replayed: %q", body)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// One inbound MSG costs a client one token and can make the hub emit
// HistoryPullBytes in reply, so /history carries its own throttle on
// top of the shared bucket.
func TestHistoryPullIsThrottled(t *testing.T) {
	h := historyHub(t, nil)
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)

	sa, _ := connect(t, h, idA)
	join(t, sa, idA, "#lobby", "")
	sa.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idA, "#lobby", "/register #lobby")))
	say(t, sa, idA, "#lobby", "something worth pulling")

	sb, linkB := connect(t, h, idB)
	join(t, sb, idB, "#lobby", "")

	sb.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idB, "#lobby", "/history #lobby 10")))
	before := len(replayedBodies(t, linkB))
	sb.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idB, "#lobby", "/history #lobby 10")))

	if got := len(replayedBodies(t, linkB)); got != before {
		t.Errorf("the second /history replayed %d more message(s), want it throttled", got-before)
	}
	var throttled bool
	for _, n := range noticesOn(t, linkB) {
		if strings.Contains(n, "rate limited") {
			throttled = true
		}
	}
	if !throttled {
		t.Error("no rate-limit notice; the client was given no reason for the silence")
	}
}
