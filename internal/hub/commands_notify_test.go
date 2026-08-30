package hub

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/peerreg"
)

// diagNotifier is a fakeNotifier that can also describe its route, the
// way the LXMF one does.
type diagNotifier struct {
	fakeNotifier
	route NotifyRoute
}

func (d *diagNotifier) DiagnoseRoute(pubKey []byte) NotifyRoute { return d.route }

// directNotifier additionally offers the direct-only route, as the LXMF
// notifier does. It records which route was taken.
type directNotifier struct {
	diagNotifier
	mu          sync.Mutex
	directCalls int
	fullCalls   int
	directErr   error
}

func (d *directNotifier) NotifyDirect(pubKey []byte, title, body string) error {
	d.mu.Lock()
	d.directCalls++
	err := d.directErr
	d.mu.Unlock()
	if err != nil {
		return err
	}
	return d.diagNotifier.NotifyAbsent(pubKey, title, body)
}

func (d *directNotifier) NotifyAbsent(pubKey []byte, title, body string) error {
	d.mu.Lock()
	d.fullCalls++
	d.mu.Unlock()
	return d.diagNotifier.NotifyAbsent(pubKey, title, body)
}

func (d *directNotifier) counts() (direct, full int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.directCalls, d.fullCalls
}

// waitForNotice polls a link until text appears in some NOTICE.
//
// /notify test answers immediately and reports the outcome from a
// goroutine, because NotifyAbsent blocks for seconds on a real mesh and
// this is the inbound frame path.
func waitForNotice(t *testing.T, link *fakeLink, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range noticesOn(t, link) {
			if strings.Contains(n, want) {
				return n
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no NOTICE containing %q arrived; got %v", want, noticesOn(t, link))
	return ""
}

// --- /notify status ---------------------------------------------------

// The complaint this whole command answers is "notifications don't
// work", made by someone who cannot see any of the five things that
// silently stop one. /notify status has to say which applies.
func TestNotifyStatusSaysWhenTheHubDoesNotNotifyAtAll(t *testing.T) {
	h := quietHub()
	s, link, id := connectKeyed(t, h, 0xA1, "alice")
	cmd(t, s, id, "", "/notify")

	if got := allNoticeText(t, link); !strings.Contains(got, "does not send mention notifications") {
		t.Errorf("/notify on a hub without the feature: %q", got)
	}
}

func TestNotifyStatusNamesTheAddressAndWhetherItAnnounced(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	n := &diagNotifier{route: NotifyRoute{
		Address:   "deadbeefdeadbeefdeadbeefdeadbeef",
		Announced: false,
		Notes:     []string{"fallback: 2 store-and-forward node(s) usable"},
	}}
	h.SetOfflineNotifier(n)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")
	cmd(t, s, id, "", "/notify status")

	got := allNoticeText(t, link)
	// The address is the one fact that lets somebody discover their
	// messaging client is a different identity from their RRC one,
	// which is the commonest cause of a notification never arriving.
	if !strings.Contains(got, "deadbeefdeadbeefdeadbeefdeadbeef") {
		t.Errorf("/notify status did not print the delivery address:\n%s", got)
	}
	if !strings.Contains(got, "not heard that address announce") {
		t.Errorf("/notify status did not report the missing announce:\n%s", got)
	}
	if !strings.Contains(got, "store-and-forward") {
		t.Errorf("/notify status dropped the notifier's notes:\n%s", got)
	}
}

// An ambiguous nick is why a mention silently resolves to nobody
// (resolveOneMentionLocked declines rather than guess). Nothing told
// the affected person until now.
func TestNotifyStatusWarnsAboutAnAmbiguousNick(t *testing.T) {
	h := mentionHub(t, nil)
	visitAndLeave(t, h, 0xB2, "sam")
	s, link, id := connectKeyed(t, h, 0xA1, "sam")
	cmd(t, s, id, "", "/notify status")

	got := allNoticeText(t, link)
	if !strings.Contains(got, "identities use that nick") {
		t.Errorf("/notify status did not warn about the duplicate nick:\n%s", got)
	}
	if !strings.Contains(got, "always works") {
		t.Errorf("/notify status did not offer the hash as the way through:\n%s", got)
	}
}

func TestNotifyStatusReportsHeldMentions(t *testing.T) {
	h := mentionHub(t, nil)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")
	h.mu.Lock()
	h.peers[hexOfID(id)].Mentions = []peerreg.Mention{{
		Room: "lobby", ByNick: "bob", Text: "@alice ping", TS: h.nowUnix(), Seq: 1,
	}}
	h.mu.Unlock()
	cmd(t, s, id, "", "/notify status")

	if got := allNoticeText(t, link); !strings.Contains(got, "1 mention(s) waiting") {
		t.Errorf("/notify status did not mention the queue:\n%s", got)
	}
}

// --- /notify on|off ---------------------------------------------------

func TestNotifyOffDiscardsPendingAndOnRestores(t *testing.T) {
	h := mentionHub(t, nil)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")
	h.mu.Lock()
	h.peers[hexOfID(id)].Mentions = []peerreg.Mention{{Room: "lobby", Text: "hi", Seq: 1}}
	h.mu.Unlock()

	cmd(t, s, id, "", "/notify off")
	if got := lastNotice(t, link); !strings.Contains(got, "off") {
		t.Errorf("/notify off: %q", got)
	}
	h.mu.Lock()
	p := h.peers[hexOfID(id)]
	optOut, pending := p.NotifyOptOut, len(p.Mentions)
	h.mu.Unlock()
	if !optOut {
		t.Error("/notify off did not record the opt-out")
	}
	if pending != 0 {
		t.Errorf("/notify off left %d mention(s) queued — they would arrive after being declined", pending)
	}

	cmd(t, s, id, "", "/notify on")
	h.mu.Lock()
	optOut = h.peers[hexOfID(id)].NotifyOptOut
	h.mu.Unlock()
	if optOut {
		t.Error("/notify on did not clear the opt-out")
	}
}

// --- /notify address --------------------------------------------------

func TestNotifyAddressPrintsTheDerivedDestination(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	h.SetOfflineNotifier(&diagNotifier{route: NotifyRoute{Address: "abc123", Announced: true}})
	s, link, id := connectKeyed(t, h, 0xA1, "alice")
	cmd(t, s, id, "", "/notify address")

	got := allNoticeText(t, link)
	if !strings.Contains(got, "abc123") {
		t.Errorf("/notify address did not print the address:\n%s", got)
	}
	if !strings.Contains(got, "different address") {
		t.Errorf("/notify address did not explain what to compare it against:\n%s", got)
	}
}

// --- /notify test -----------------------------------------------------

// The test must take the production path, not a simulation of it: three
// separate incidents here were fixes that each worked and changed
// nothing at the far end.
func TestNotifyTestSendsThroughTheRealNotifier(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	n := &diagNotifier{}
	h.SetOfflineNotifier(n)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")

	cmd(t, s, id, "", "/notify test")
	waitForNotice(t, link, "delivered")

	if got := n.delivered(); len(got) != 1 {
		t.Fatalf("notifier saw %d sends, want 1", len(got))
	}
}

// An acknowledgement that is not a receipt must not be reported as one:
// a propagation node acknowledges storage, never delivery.
func TestNotifyTestDistinguishesAnUnconfirmedSend(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	n := &diagNotifier{}
	n.fail = fmt.Errorf("%w: uploaded to 2 node(s)", ErrDeliveredUnconfirmed)
	h.SetOfflineNotifier(n)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")

	cmd(t, s, id, "", "/notify test")
	got := waitForNotice(t, link, "nothing confirms")
	if strings.Contains(got, "works for you") {
		t.Errorf("an unconfirmed send was reported as success: %q", got)
	}
	if !strings.Contains(got, "/notify address") {
		t.Errorf("an unconfirmed send did not point at the likeliest cause: %q", got)
	}
}

// "Could not try" is not "tried and failed" — the same distinction the
// push throttle is built on.
func TestNotifyTestDistinguishesNoRouteAtAll(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	n := &diagNotifier{}
	n.fail = fmt.Errorf("%w: no propagation node", ErrNotifierUnavailable)
	h.SetOfflineNotifier(n)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")

	cmd(t, s, id, "", "/notify test")
	waitForNotice(t, link, "no route to even try")
}

func TestNotifyTestIsThrottled(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	n := &diagNotifier{}
	h.SetOfflineNotifier(n)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")

	cmd(t, s, id, "", "/notify test")
	waitForNotice(t, link, "delivered")
	cmd(t, s, id, "", "/notify test")
	waitForNotice(t, link, "already tested recently")

	if got := n.calls; got != 1 {
		t.Errorf("the notifier was called %d times; the throttle must stop a second real send", got)
	}
}

func TestNotifyTestSaysSoWhenThereIsNoNotifier(t *testing.T) {
	h := mentionHub(t, nil)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")
	cmd(t, s, id, "", "/notify test")

	if got := allNoticeText(t, link); !strings.Contains(got, "held until you next connect") {
		t.Errorf("/notify test without a notifier: %q", got)
	}
}

// --- /mentions --------------------------------------------------------

func TestMentionsListsAndClearsTheQueue(t *testing.T) {
	h := mentionHub(t, nil)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")
	h.mu.Lock()
	h.peers[hexOfID(id)].Mentions = []peerreg.Mention{
		{Room: "lobby", ByNick: "bob", Text: "@alice ping", TS: h.nowUnix(), Seq: 1},
	}
	h.mu.Unlock()

	cmd(t, s, id, "", "/mentions")
	got := allNoticeText(t, link)
	if !strings.Contains(got, "@alice ping") || !strings.Contains(got, "bob") {
		t.Errorf("/mentions did not show the pending mention:\n%s", got)
	}

	cmd(t, s, id, "", "/mentions clear")
	h.mu.Lock()
	left := len(h.peers[hexOfID(id)].Mentions)
	h.mu.Unlock()
	if left != 0 {
		t.Errorf("/mentions clear left %d", left)
	}
}

// --- /away ------------------------------------------------------------

// The hub's presence test can only measure whether frames arrive, so a
// client connected in a background tab looks exactly like someone
// reading the room — and a mention in that state is written into the
// room and nowhere else. /away is the only thing that can tell it
// otherwise.
func TestAwayMakesAMentionQueueRatherThanVanishIntoTheRoom(t *testing.T) {
	h := mentionHub(t, nil)
	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	sb, _, idB := connectKeyed(t, h, 0xB2, "bob")
	join(t, sa, idA, "lobby", "")
	join(t, sb, idB, "lobby", "")

	cmd(t, sb, idB, "", "/away making tea")
	if !sb.isAway() {
		t.Fatal("/away did not mark the session away")
	}

	say(t, sa, idA, "lobby", "@bob are you there")

	held := pendingFor(h, idB)
	if len(held) != 1 {
		t.Fatalf("an away peer's mention was not queued: %v", held)
	}
}

func TestAwayPeerIsPushedToEvenWhileConnected(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	n := &diagNotifier{}
	h.SetOfflineNotifier(n)
	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	sb, _, idB := connectKeyed(t, h, 0xB2, "bob")
	join(t, sa, idA, "lobby", "")
	join(t, sb, idB, "lobby", "")
	cmd(t, sb, idB, "", "/away")
	say(t, sa, idA, "lobby", "@bob are you there")

	h.pushPendingMentions()
	if got := len(n.delivered()); got != 1 {
		t.Errorf("an away peer was not pushed to (%d sends) — the RRC copy goes to a screen nobody is reading", got)
	}
}

func TestBackHandsOverWhatArrivedWhileAway(t *testing.T) {
	h := mentionHub(t, nil)
	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	sb, linkB, idB := connectKeyed(t, h, 0xB2, "bob")
	join(t, sa, idA, "lobby", "")
	join(t, sb, idB, "lobby", "")
	cmd(t, sb, idB, "", "/away")
	say(t, sa, idA, "lobby", "@bob urgent")

	cmd(t, sb, idB, "", "/back")
	if sb.isAway() {
		t.Fatal("/back did not clear away")
	}
	if got := allNoticeText(t, linkB); !strings.Contains(got, "urgent") {
		t.Errorf("/back did not hand over what arrived while away:\n%s", got)
	}
	if left := pendingFor(h, idB); len(left) != 0 {
		t.Errorf("%d mention(s) still queued after /back", len(left))
	}
}

// --- /whoami, /seen ---------------------------------------------------

func TestWhoamiNamesTheIdentityAndHowToBeMentioned(t *testing.T) {
	h := mentionHub(t, nil)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")
	join(t, s, id, "lobby", "")
	cmd(t, s, id, "", "/whoami")

	got := allNoticeText(t, link)
	if !strings.Contains(got, hexOfID(id)) {
		t.Errorf("/whoami did not print the identity hash:\n%s", got)
	}
	if !strings.Contains(got, "alice") || !strings.Contains(got, "lobby") {
		t.Errorf("/whoami did not print the nick and rooms:\n%s", got)
	}
}

func TestSeenReportsBothConnectedAndDepartedPeers(t *testing.T) {
	h := mentionHub(t, nil)
	gone := visitAndLeave(t, h, 0xB2, "bob")
	s, link, id := connectKeyed(t, h, 0xA1, "alice")

	cmd(t, s, id, "", "/seen bob")
	got := allNoticeText(t, link)
	if !strings.Contains(got, hexOfID(gone)) {
		t.Errorf("/seen did not find a departed peer:\n%s", got)
	}
}

// Two people with one nick is the reason a mention resolves to nobody.
// /seen is where somebody can find out that has happened to them.
func TestSeenSurfacesNickAmbiguity(t *testing.T) {
	h := mentionHub(t, nil)
	visitAndLeave(t, h, 0xB2, "sam")
	visitAndLeave(t, h, 0xC3, "sam")
	s, link, id := connectKeyed(t, h, 0xA1, "alice")

	cmd(t, s, id, "", "/seen sam")
	if got := allNoticeText(t, link); !strings.Contains(got, "resolves to nobody") {
		t.Errorf("/seen did not flag the ambiguity:\n%s", got)
	}
}

func TestSeenOnNobody(t *testing.T) {
	h := mentionHub(t, nil)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")
	cmd(t, s, id, "", "/seen nobody")
	if got := allNoticeText(t, link); !strings.Contains(got, "matches") {
		t.Errorf("/seen on an unknown name: %q", got)
	}
}

// hexOfID renders an identity hash as the directory key.
func hexOfID(id []byte) string { return hex.EncodeToString(id) }

// /notify test must answer while the person who typed it is still
// connected. The full path cannot: it tries several propagation nodes,
// each costing up to 20 seconds on an LRPROOF timeout. Measured on a
// public mesh at 45 seconds, by which point the client had gone and the
// answer was written into a dead link — the user saw "the result
// follows" and then nothing.
func TestNotifyTestUsesTheDirectRouteOnly(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	n := &directNotifier{}
	h.SetOfflineNotifier(n)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")

	cmd(t, s, id, "", "/notify test")
	waitForNotice(t, link, "delivered")

	direct, full := n.counts()
	if direct != 1 {
		t.Errorf("direct route used %d times, want 1", direct)
	}
	if full != 0 {
		t.Errorf("the store-and-forward fallback ran %d times; it cannot confirm delivery and costs the answer its timeliness", full)
	}
}

// Nothing is lost by skipping the fallback, but the reply has to say so
// — otherwise "no direct route" reads as "you will never be notified".
func TestAFailedDirectTestExplainsTheFallbackStillExists(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	n := &directNotifier{}
	n.directErr = errors.New("no delivery proof from recipient after 10s")
	h.SetOfflineNotifier(n)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")

	cmd(t, s, id, "", "/notify test")
	got := waitForNotice(t, link, "no direct route")
	if !strings.Contains(got, "store-and-forward") {
		t.Errorf("reply does not say a real mention still has a fallback: %q", got)
	}
	if !strings.Contains(got, "/notify address") {
		t.Errorf("reply does not point at the likeliest cause: %q", got)
	}
}

// A notifier without the direct capability still works, on the full
// path, and the reply sets the slower expectation.
func TestNotifyTestFallsBackToTheFullPathWhenDirectIsUnavailable(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	n := &diagNotifier{}
	h.SetOfflineNotifier(n)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")

	cmd(t, s, id, "", "/notify test")
	if got := allNoticeText(t, link); !strings.Contains(got, "up to a minute") {
		t.Errorf("the slower path did not say so: %s", got)
	}
	waitForNotice(t, link, "delivered")
	if got := len(n.delivered()); got != 1 {
		t.Errorf("notifier saw %d sends, want 1", got)
	}
}

// "full" is the operator's question — can this hub reach a propagation
// node at all? — which the direct route cannot answer and which has no
// other way to be asked.
func TestNotifyTestFullUsesTheWholeProductionPath(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	n := &directNotifier{}
	h.SetOfflineNotifier(n)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")

	cmd(t, s, id, "", "/notify test full")
	waitForNotice(t, link, "delivered")

	direct, full := n.counts()
	if full != 1 {
		t.Errorf("full path used %d times, want 1", full)
	}
	if direct != 0 {
		t.Errorf("direct-only route used %d times on an explicit full test, want 0", direct)
	}
}

// The slow path can outlive the connection, which is the whole reason
// it is not the default. Say so before they wait.
func TestNotifyTestFullWarnsItMayOutliveTheConnection(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	h.SetOfflineNotifier(&directNotifier{})
	s, link, id := connectKeyed(t, h, 0xA1, "alice")

	cmd(t, s, id, "", "/notify test full")
	got := allNoticeText(t, link)
	if !strings.Contains(got, "stay connected") {
		t.Errorf("the slow path did not warn the answer may be missed:\n%s", got)
	}
}

// A failed full test must not blame the direct route alone — it tried
// everything, and saying otherwise sends the operator hunting the wrong
// thing.
func TestAFailedFullTestSaysEveryRouteFailed(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	n := &directNotifier{}
	n.fail = errors.New("no propagation node accepted it")
	h.SetOfflineNotifier(n)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")

	cmd(t, s, id, "", "/notify test full")
	got := waitForNotice(t, link, "every route failed")
	if strings.Contains(got, "no direct route") {
		t.Errorf("a full test blamed the direct route alone: %q", got)
	}
}

// And the default still points at the slow path as the next step.
func TestAFailedDirectTestOffersTheFullTest(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MentionLXMF = true })
	n := &directNotifier{}
	n.directErr = errors.New("no delivery proof after 10s")
	h.SetOfflineNotifier(n)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")

	cmd(t, s, id, "", "/notify test")
	got := waitForNotice(t, link, "no direct route")
	if !strings.Contains(got, "/notify test full") {
		t.Errorf("reply does not offer the fallback test: %q", got)
	}
}
