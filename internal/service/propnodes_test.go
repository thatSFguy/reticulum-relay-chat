package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/thatSFguy/reticulum-go/lxmf"
	"github.com/thatSFguy/reticulum-go/rns"
)

// nodeAppData builds a §5.8.5 announce payload: the strict 7-element
// msgpack array a real propagation node announces.
func nodeAppData(enabled bool, transferLimitKB, stampCost int) []byte {
	b, err := msgpack.Marshal([]any{
		nil, time.Now().Unix(), enabled, transferLimitKB, 0,
		[]any{stampCost, 0, 0}, nil,
	})
	if err != nil {
		panic(err)
	}
	return b
}

// nodeFixture is one propagation node in a test: a real identity (the
// transport's Restore re-derives the destination from the public key
// before accepting a cache entry, so a made-up hash is refused), its
// lxmf.propagation destination, and the app_data it announced.
type nodeFixture struct {
	id      *rns.Identity
	dest    []byte
	appData []byte
}

func fakeNode(t *testing.T, enabled bool, transferLimitKB, stampCost int) nodeFixture {
	t.Helper()
	id, err := rns.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	return nodeFixture{
		id:      id,
		dest:    id.DestinationHashFor(lxmf.PropagationFullName()),
		appData: nodeAppData(enabled, transferLimitKB, stampCost),
	}
}

// trackerWith builds a propNodes whose clock is under test control, and
// feeds it announces for each node — both into the tracker and into the
// transport's cache, because Select re-reads the cache as the authority
// on a node's current parameters.
func trackerWith(t *testing.T, now *time.Time, heardAgo map[byte]time.Duration, nodes ...nodeFixture) *propNodes {
	t.Helper()
	tr := rns.NewTransport(log.New(io.Discard, "", 0))
	p := newPropNodes(log.New(io.Discard, "", 0), tr, nil)
	p.now = func() time.Time { return *now }

	for i, n := range nodes {
		tr.Restore(&rns.KnownIdentity{
			DestHash:  n.dest,
			PublicKey: n.id.PublicKey(),
			NameHash:  lxmfPropagationNameHash,
			AppData:   n.appData,
		})
		// Rewind the clock so the node's first announce lands in the
		// past, then restore it: firstHeard is what selection ranks on.
		real := *now
		if ago, ok := heardAgo[byte(i)]; ok {
			*now = real.Add(-ago)
		}
		p.OnAnnounce(&rns.Announce{DestHash: n.dest, AppData: n.appData})
		*now = real
		// A second announce at the current time keeps lastHeard fresh
		// without moving firstHeard.
		p.OnAnnounce(&rns.Announce{DestHash: n.dest, AppData: n.appData})
	}
	return p
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// The node known LONGEST wins, not the one heard most recently.
//
// Announces are unauthenticated and free, so most-recently-heard is
// trivially gameable: anyone announcing once per second beats honest
// nodes announcing on a multi-minute schedule, seizes the role, and
// blackholes every mention. Longest-known makes an attacker out-persist
// the field rather than out-shout it.
func TestSelectPrefersTheLongestKnownNode(t *testing.T) {
	now := time.Now()
	established := fakeNode(t, true, 0, 0)
	shouty := fakeNode(t, true, 0, 0)
	p := trackerWith(t, &now, map[byte]time.Duration{
		0: 4 * time.Hour,   // established
		1: 3 * time.Minute, // heard recently, but old enough to qualify
	}, established, shouty)

	got := p.Select(1)
	if len(got) != 1 {
		t.Fatalf("Select returned %d nodes, want 1", len(got))
	}
	if hexOf(got[0]) != hexOf(established.dest) {
		t.Errorf("selected the newcomer; a node announcing more often must not displace an established one")
	}
}

// A node announced seconds ago has demonstrated nothing.
func TestSelectSkipsANodeItHasOnlyJustHeard(t *testing.T) {
	now := time.Now()
	fresh := fakeNode(t, true, 0, 0)
	p := trackerWith(t, &now, map[byte]time.Duration{0: 5 * time.Second}, fresh)

	if got := p.Select(1); len(got) != 0 {
		t.Errorf("selected %d node(s) heard 5s ago; nodeMinAge is %v", len(got), nodeMinAge)
	}
}

// A node that has stopped announcing must stop being a candidate,
// rather than staying selectable forever on one announce.
func TestSelectDropsAStaleNode(t *testing.T) {
	now := time.Now()
	n := fakeNode(t, true, 0, 0)
	p := trackerWith(t, &now, map[byte]time.Duration{0: time.Hour}, n)

	if got := p.Select(1); len(got) != 1 {
		t.Fatalf("setup: node should be selectable, got %d", len(got))
	}
	now = now.Add(nodeStaleAfter + time.Minute)
	if got := p.Select(1); len(got) != 0 {
		t.Errorf("selected %d stale node(s) after %v of silence", len(got), nodeStaleAfter)
	}
}

// Parameters we cannot satisfy disqualify a node at SELECTION time.
//
// Discovering them inside SendPropagated means the upload was already
// attempted and the mention already burned a push interval on a node
// that was never going to work — which is a cheap way for a hostile
// node to swallow other people's mail.
func TestSelectRejectsUnusableParameters(t *testing.T) {
	cases := []struct {
		name string
		node nodeFixture
	}{
		{"not accepting messages", fakeNode(t, false, 0, 0)},
		{"implausibly small transfer limit", fakeNode(t, true, 1, 0)},
		{"stamp cost above our limit", fakeNode(t, true, 0, lxmf.MaxPropagationStampCost+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			p := trackerWith(t, &now, map[byte]time.Duration{0: time.Hour}, tc.node)
			if got := p.Select(1); len(got) != 0 {
				t.Errorf("selected a node that %s", tc.name)
			}
		})
	}
}

// Reachability is what actually broke the live test ("LRPROOF did not
// arrive before timeout"), and a completed upload is the only direct
// evidence of it. A node that has worked outranks one that has not,
// even one known far longer.
func TestSelectPrefersANodeThatHasWorked(t *testing.T) {
	now := time.Now()
	older := fakeNode(t, true, 0, 0)
	proven := fakeNode(t, true, 0, 0)
	p := trackerWith(t, &now, map[byte]time.Duration{
		0: 6 * time.Hour,
		1: 10 * time.Minute,
	}, older, proven)

	if got := p.Select(1); hexOf(got[0]) != hexOf(older.dest) {
		t.Fatal("setup: the longest-known node should win before any upload evidence")
	}
	p.RecordResult(proven.dest, nil)

	got := p.Select(1)
	if len(got) == 0 || hexOf(got[0]) != hexOf(proven.dest) {
		t.Error("a node we have successfully uploaded to must outrank one we have only heard announce")
	}
}

// A recent failure demotes a node below untried ones...
func TestSelectDemotesANodeThatFailed(t *testing.T) {
	now := time.Now()
	failing := fakeNode(t, true, 0, 0)
	untried := fakeNode(t, true, 0, 0)
	p := trackerWith(t, &now, map[byte]time.Duration{
		0: 6 * time.Hour,
		1: 10 * time.Minute,
	}, failing, untried)

	p.RecordResult(failing.dest, errNodeUnreachable)

	got := p.Select(1)
	if len(got) == 0 || hexOf(got[0]) != hexOf(untried.dest) {
		t.Error("a node that just failed us must not be picked ahead of an untried one")
	}
}

// ...but does not disqualify it. On a hub that has only ever reached one
// node, "the node that failed us twenty minutes ago" beats "nowhere".
func TestAFailedNodeIsStillBetterThanNoNode(t *testing.T) {
	now := time.Now()
	only := fakeNode(t, true, 0, 0)
	p := trackerWith(t, &now, map[byte]time.Duration{0: 6 * time.Hour}, only)

	p.RecordResult(only.dest, errNodeUnreachable)

	if got := p.Select(1); len(got) != 1 {
		t.Errorf("Select returned %d nodes; a demoted node is still the only route there is", len(got))
	}
}

// The hub cannot know which node the recipient syncs from, so the
// fallback route leaves the message with several.
func TestSelectFansOutToSeveralNodes(t *testing.T) {
	now := time.Now()
	a, b, c := fakeNode(t, true, 0, 0), fakeNode(t, true, 0, 0), fakeNode(t, true, 0, 0)
	p := trackerWith(t, &now, map[byte]time.Duration{
		0: 6 * time.Hour, 1: 5 * time.Hour, 2: 4 * time.Hour,
	}, a, b, c)

	got := p.Select(2)
	if len(got) != 2 {
		t.Fatalf("Select(2) returned %d nodes, want 2", len(got))
	}
	if hexOf(got[0]) == hexOf(got[1]) {
		t.Error("Select returned the same node twice")
	}
	// Best-first: the two known longest.
	if hexOf(got[0]) != hexOf(a.dest) || hexOf(got[1]) != hexOf(b.dest) {
		t.Error("fan-out must still be ordered best-first")
	}
}

// A pinned node is an operator's trust decision. Failing over to a
// stranger's node — or quietly fanning out to several — would undo it.
func TestAPinnedNodeIsUsedAloneAndUnconditionally(t *testing.T) {
	tr := rns.NewTransport(log.New(io.Discard, "", 0))
	pinned := fakeNode(t, true, 0, 0)
	p := newPropNodes(log.New(io.Discard, "", 0), tr, pinned.dest)

	// Never announced, never heard, and asked to fan out to three.
	got := p.Select(3)
	if len(got) != 1 {
		t.Fatalf("Select returned %d nodes, want just the pinned one", len(got))
	}
	if hexOf(got[0]) != hexOf(pinned.dest) {
		t.Errorf("selected %x, want the pinned %x", got[0], pinned.dest)
	}
}

// Announces are free and unauthenticated, so the discovered-node map
// needs a bound or anyone cycling identities grows it without limit.
func TestTrackedNodesAreBounded(t *testing.T) {
	now := time.Now()
	tr := rns.NewTransport(log.New(io.Discard, "", 0))
	p := newPropNodes(log.New(io.Discard, "", 0), tr, nil)
	p.now = func() time.Time { return now }

	appData := nodeAppData(true, 0, 0)
	for i := 0; i < maxTrackedNodes*3; i++ {
		h := sha256.Sum256([]byte{byte(i), byte(i >> 8), 0xAA})
		p.OnAnnounce(&rns.Announce{DestHash: h[:rns.IdentityHashLen], AppData: appData})
	}

	p.mu.Lock()
	n := len(p.nodes)
	p.mu.Unlock()
	if n > maxTrackedNodes {
		t.Errorf("tracking %d nodes, cap is %d", n, maxTrackedNodes)
	}
}

// errNodeUnreachable stands in for the live failure this policy exists
// to react to: "LRPROOF did not arrive before timeout".
var errNodeUnreachable = errors.New("LRPROOF did not arrive before timeout")
