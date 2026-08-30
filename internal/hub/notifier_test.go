package hub

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
)

// fakeNotifier records what the hub tried to send, and can be made to
// fail — the case that matters most, since reaching an absent peer over
// LXMF is best-effort.
type fakeNotifier struct {
	mu    sync.Mutex
	sent  []fakeNotification
	fail  error
	calls int
}

type fakeNotification struct {
	pubKey []byte
	title  string
	body   string
}

func (f *fakeNotifier) NotifyAbsent(pubKey []byte, title, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil {
		return f.fail
	}
	f.sent = append(f.sent, fakeNotification{
		pubKey: append([]byte(nil), pubKey...),
		title:  title,
		body:   body,
	})
	return nil
}

func (f *fakeNotifier) delivered() []fakeNotification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeNotification(nil), f.sent...)
}

func (f *fakeNotifier) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// queueMentionFor arranges for one mention to be waiting for an absent
// peer, and returns that peer's identity and key.
func queueMentionFor(t *testing.T, h *Hub, text string) (id, pubKey []byte) {
	t.Helper()
	pubKey, id = keyFor(0xB2)
	visitAndLeave(t, h, 0xB2, "bob")

	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "lobby", "")
	say(t, sa, idA, "lobby", "@bob "+text)

	if len(pendingFor(h, id)) != 1 {
		t.Fatalf("setup: the mention did not queue")
	}
	return id, pubKey
}

// The point of the whole path: a mention for someone who is gone leaves
// the hub, addressed by the key they proved when they were here.
func TestAWaitingMentionIsPushedToTheNotifier(t *testing.T) {
	h := mentionHub(t, nil)
	n := &fakeNotifier{}
	h.SetOfflineNotifier(n)

	id, pubKey := queueMentionFor(t, h, "are you there?")
	h.pushPendingMentions()

	sent := n.delivered()
	if len(sent) != 1 {
		t.Fatalf("notifier saw %d notifications, want 1", len(sent))
	}
	if !equalBytesHelper(sent[0].pubKey, pubKey) {
		t.Errorf("addressed to key %x, want the peer's %x", sent[0].pubKey, pubKey)
	}
	if !strings.Contains(sent[0].title, "lobby") {
		t.Errorf("title %q does not say where it came from", sent[0].title)
	}
	if !strings.Contains(sent[0].body, "are you there?") || !strings.Contains(sent[0].body, "alice") {
		t.Errorf("body %q does not carry the message or its author", sent[0].body)
	}

	// Delivered mentions are cleared, so the peer is not told twice.
	if got := pendingFor(h, id); len(got) != 0 {
		t.Errorf("%d mentions still queued after a successful push", len(got))
	}
}

// Failure is the common case — no node reachable, or a peer whose LXMF
// identity differs from their RRC one — and must leave the fallback
// intact.
func TestAFailedPushLeavesTheMentionQueued(t *testing.T) {
	h := mentionHub(t, nil)
	n := &fakeNotifier{fail: errors.New("no propagation node available")}
	h.SetOfflineNotifier(n)

	id, _ := queueMentionFor(t, h, "still waiting")
	h.pushPendingMentions()

	if got := pendingFor(h, id); len(got) != 1 {
		t.Fatalf("%d mentions queued after a failed push, want the original still waiting", len(got))
	}

	// And the RRC fallback still works: the peer is told on return.
	_, linkB, _ := connectKeyed(t, h, 0xB2, "bob")
	if !hasSubstringIn(noticesOn(t, linkB), "still waiting") {
		t.Errorf("the mention was not delivered over RRC after the push failed: %v", noticesOn(t, linkB))
	}
}

// A peer who is connected hears about it over RRC, so pushing would
// duplicate the notification.
func TestAConnectedPeerIsNotPushed(t *testing.T) {
	h := mentionHub(t, nil)
	n := &fakeNotifier{}
	h.SetOfflineNotifier(n)

	id, _ := queueMentionFor(t, h, "hello")
	// Bob comes back; his session drains the queue at HELLO.
	connectKeyed(t, h, 0xB2, "bob")
	if len(pendingFor(h, id)) != 0 {
		t.Fatal("setup: reconnecting did not drain the queue")
	}

	h.pushPendingMentions()
	if n.attempts() != 0 {
		t.Errorf("pushed %d notifications for a connected peer", n.attempts())
	}
}

// A propagation upload is expensive and a peer who is away stays away,
// so a repeated sweep must not re-send what a node already holds.
func TestPushesAreThrottledPerPeer(t *testing.T) {
	h := mentionHub(t, nil)
	n := &fakeNotifier{fail: errors.New("still no node")}
	h.SetOfflineNotifier(n)

	queueMentionFor(t, h, "one")
	h.pushPendingMentions()
	h.pushPendingMentions()
	h.pushPendingMentions()

	if n.attempts() != 1 {
		t.Errorf("made %d attempts across three sweeps, want 1", n.attempts())
	}
}

// Opting out means opting out of everything, including the path that
// leaves the hub.
func TestAnOptedOutPeerIsNeverPushed(t *testing.T) {
	h := mentionHub(t, nil)
	n := &fakeNotifier{}
	h.SetOfflineNotifier(n)

	id, _ := queueMentionFor(t, h, "hello")

	h.mu.Lock()
	h.peers[hex.EncodeToString(id)].NotifyOptOut = true
	h.mu.Unlock()

	h.pushPendingMentions()
	if n.attempts() != 0 {
		t.Errorf("pushed %d notifications for a peer who opted out", n.attempts())
	}
}

// Several waiting mentions become one message: a client should get one
// notification saying "you were named three times", not three.
func TestMultipleWaitingMentionsBecomeOneNotification(t *testing.T) {
	h := mentionHub(t, nil)
	n := &fakeNotifier{}
	h.SetOfflineNotifier(n)

	visitAndLeave(t, h, 0xB2, "bob")
	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "lobby", "")
	for _, text := range []string{"first", "second", "third"} {
		say(t, sa, idA, "lobby", "@bob "+text)
	}

	h.pushPendingMentions()

	sent := n.delivered()
	if len(sent) != 1 {
		t.Fatalf("notifier saw %d notifications, want them batched into 1", len(sent))
	}
	if !strings.Contains(sent[0].title, "3 mentions") {
		t.Errorf("title %q does not say how many", sent[0].title)
	}
	for _, want := range []string{"first", "second", "third"} {
		if !strings.Contains(sent[0].body, want) {
			t.Errorf("body is missing %q: %s", want, sent[0].body)
		}
	}
}

// A mention that arrives while an upload is in flight must survive it:
// clearing the whole queue on success would silently drop it.
func TestAMentionArrivingDuringAPushIsNotLost(t *testing.T) {
	h := mentionHub(t, nil)
	var sa *Session
	var idA []byte
	n := &notifierHook{onNotify: func() {
		// Racing the upload: a second mention lands mid-flight.
		say(t, sa, idA, "lobby", "@bob second")
	}}
	h.SetOfflineNotifier(n)

	_, id := keyFor(0xB2)
	visitAndLeave(t, h, 0xB2, "bob")
	sa, _, idA = connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "lobby", "")
	say(t, sa, idA, "lobby", "@bob first")

	h.pushPendingMentions()

	held := pendingFor(h, id)
	if len(held) != 1 {
		t.Fatalf("%d mentions queued, want the one that arrived mid-push", len(held))
	}
	if !strings.Contains(held[0].Text, "second") {
		t.Errorf("surviving mention is %q, want the one that arrived during the push", held[0].Text)
	}
}

// notifierHook runs a callback during delivery, to model something
// happening while an upload is in flight.
type notifierHook struct {
	onNotify func()
}

func (n *notifierHook) NotifyAbsent(pubKey []byte, title, body string) error {
	if n.onNotify != nil {
		n.onNotify()
	}
	return nil
}

// With mentions off there is nothing to push, notifier or not.
func TestNothingIsPushedWhenMentionsAreOff(t *testing.T) {
	h := quietHub()
	n := &fakeNotifier{}
	h.SetOfflineNotifier(n)

	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "lobby", "")
	say(t, sa, idA, "lobby", "@bob hello")
	h.pushPendingMentions()

	if n.attempts() != 0 {
		t.Errorf("pushed %d notifications from a hub with mentions off", n.attempts())
	}
}

func TestRenderMentionNotificationNamesTheHub(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.Name = "Michigan Mesh" })
	n := &fakeNotifier{}
	h.SetOfflineNotifier(n)

	queueMentionFor(t, h, "ping")
	h.pushPendingMentions()

	sent := n.delivered()
	if len(sent) != 1 {
		t.Fatalf("got %d notifications", len(sent))
	}
	// The message lands in a general-purpose LXMF client beside
	// unrelated conversations, so it has to say where it came from.
	if !strings.HasPrefix(sent[0].title, "Michigan Mesh") {
		t.Errorf("title %q does not name the hub", sent[0].title)
	}
}

func equalBytesHelper(a, b []byte) bool {
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

// The queue evicts from the front when it is full, so index arithmetic
// on it is only safe while nothing has been evicted. This is the case
// where something has: a full queue, an upload in flight, and more
// mentions arriving before it returns. Trimming by count discards the
// new arrivals along with the sent ones.
func TestAFullQueueDoesNotLoseMentionsArrivingDuringAPush(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MaxPendingMentions = 3 })
	var sa *Session
	var idA []byte
	n := &notifierHook{onNotify: func() {
		// Overflows the queue: "one" is evicted to make room, so the
		// three that were sent are no longer at the front.
		say(t, sa, idA, "lobby", "@bob four")
	}}
	h.SetOfflineNotifier(n)

	_, id := keyFor(0xB2)
	visitAndLeave(t, h, 0xB2, "bob")
	sa, _, idA = connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "lobby", "")
	for _, word := range []string{"one", "two", "three"} {
		say(t, sa, idA, "lobby", "@bob "+word)
	}
	if got := len(pendingFor(h, id)); got != 3 {
		t.Fatalf("setup: %d mentions queued, want the queue full at 3", got)
	}

	h.pushPendingMentions()

	held := pendingFor(h, id)
	if len(held) != 1 {
		t.Fatalf("%d mentions queued, want only the one that arrived mid-push", len(held))
	}
	if !strings.Contains(held[0].Text, "four") {
		t.Errorf("surviving mention is %q, want the one that arrived during the push", held[0].Text)
	}
}

// A propagation upload is acknowledged by the NODE, never by the
// recipient. When the hub could not stamp the message — it has never
// heard that peer announce, so it does not know their stamp_cost — a
// recipient enforcing stamps discards it in silence. Dropping the
// mention on the strength of that upload destroys the RRC fallback too,
// and the notification is simply lost.
func TestAnUnstampedUploadKeepsTheMentionQueued(t *testing.T) {
	h := mentionHub(t, nil)
	h.SetOfflineNotifier(&fakeNotifier{fail: ErrDeliveredUnconfirmed})

	id, _ := queueMentionFor(t, h, "are you there")
	h.pushPendingMentions()

	held := pendingFor(h, id)
	if len(held) != 1 {
		t.Fatalf("%d mentions queued, want the mention kept for RRC after an unstamped upload", len(held))
	}
}

// The confident case is unchanged: a stamped upload is trusted, and a
// second copy over RRC would be a duplicate.
func TestAStampedUploadStillClearsTheQueue(t *testing.T) {
	h := mentionHub(t, nil)
	h.SetOfflineNotifier(&fakeNotifier{})

	id, _ := queueMentionFor(t, h, "are you there")
	h.pushPendingMentions()

	if held := pendingFor(h, id); len(held) != 0 {
		t.Fatalf("%d mentions still queued after a stamped upload, want none", len(held))
	}
}

// pinRecorder captures the pinned set the hub asserts.
type pinRecorder struct {
	fakeNotifier
	mu     sync.Mutex
	pinned [][]byte
}

func (p *pinRecorder) PinPeers(keys [][]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pinned = append([][]byte(nil), keys...)
}

func (p *pinRecorder) pinnedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pinned)
}

// The recipient's stamp_cost lives only in their cached announce, and
// that cache evicts the oldest unpinned entry. A hub that never pins is
// one busy afternoon away from losing the key material it needs to send
// a deliverable notification.
func TestKnownPeersArePinnedAgainstCacheEviction(t *testing.T) {
	h := mentionHub(t, nil)
	rec := &pinRecorder{}
	h.SetOfflineNotifier(rec)

	visitAndLeave(t, h, 0xB2, "bob")
	if got := rec.pinnedCount(); got != 1 {
		t.Fatalf("pinned %d peers after one visit, want 1", got)
	}

	visitAndLeave(t, h, 0xC3, "carol")
	if got := rec.pinnedCount(); got != 2 {
		t.Fatalf("pinned %d peers after two visits, want 2", got)
	}

	_, id := keyFor(0xB2)
	h.mu.Lock()
	want := append([]byte(nil), h.peers[hex.EncodeToString(id)].PublicKey...)
	h.mu.Unlock()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var found bool
	for _, k := range rec.pinned {
		if bytes.Equal(k, want) {
			found = true
		}
	}
	if !found {
		t.Error("bob's key is not in the pinned set")
	}
}

// A notifier that does not implement the capability must still work.
func TestPinningIsOptional(t *testing.T) {
	h := mentionHub(t, nil)
	h.SetOfflineNotifier(&fakeNotifier{})
	visitAndLeave(t, h, 0xB2, "bob") // must not panic
	id, _ := queueMentionFor(t, h, "hello")
	h.pushPendingMentions()
	if held := pendingFor(h, id); len(held) != 0 {
		t.Errorf("%d mentions still queued; delivery broke without the pin capability", len(held))
	}
}
