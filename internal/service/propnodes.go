package service

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/thatSFguy/reticulum-go/lxmf"
	"github.com/thatSFguy/reticulum-go/rns"
)

// Choosing a propagation node to leave a mention with.
//
// This is the weakest link in the offline path and it is worth being
// honest about why. A propagation node is store-and-forward: the hub
// hands it a message and the recipient's own client collects it on its
// next sync. But a client syncs from the node ITS operator configured,
// and nothing in an announce says which one that is. So the hub is
// picking from a crowd — better than seventy nodes announce on a busy
// public mesh in five minutes — with no way to tell which of them the
// recipient will ever talk to.
//
// Nothing here fixes that; it cannot be fixed from this side. What it
// does is stop making it worse:
//
//   - Only nodes whose announced parameters we can actually satisfy are
//     candidates (see acceptable). Previously an unusable value was
//     discovered inside SendPropagated, after the upload had already
//     been attempted.
//   - A node that has gone quiet stops being a candidate, instead of
//     staying selectable forever on the strength of one announce.
//   - Among the rest, the node we have known LONGEST wins. Announces
//     are unauthenticated and free, so preferring the most recently
//     heard node lets anyone announcing once per second deterministically
//     seize the role from established nodes and blackhole every mention.
//     Longest-known makes an attacker out-persist the field rather than
//     out-shout it.
//   - Nodes we have successfully uploaded to are preferred, and ones
//     that failed us are demoted. Reachability is the property that
//     actually broke the live test ("LRPROOF did not arrive before
//     timeout"), and a completed upload is the only direct evidence of
//     it available.
//
// And because none of that can identify the RIGHT node, Select returns
// several: see the fan-out note on Config.LXMFPropagationFanout.

var lxmfPropagationNameHash = rns.NameHash(lxmf.PropagationFullName())

// Node-selection policy.
const (
	// maxTrackedNodes bounds the discovered-node map. Announces are
	// unauthenticated, so without a cap anyone cycling identities grows
	// it without limit — and Select scans it on every notification.
	maxTrackedNodes = 256

	// nodeStaleAfter drops a node that has not re-announced. Without it
	// a node heard once, hours ago, stays selectable forever.
	nodeStaleAfter = 30 * time.Minute

	// nodeMinAge is how long a freshly-heard node must have been known
	// before auto-selection will use it. A node announced seconds ago
	// has demonstrated nothing.
	nodeMinAge = 2 * time.Minute

	// minTransferLimitKB rejects a node advertising an implausibly small
	// per-transfer cap — announce 1 KB and every upload fails its size
	// check, which is a cheap way to swallow other people's mail.
	minTransferLimitKB = 16

	// nodeFailureCooldown is how long a failed upload demotes a node.
	// Long enough that a node that is simply unreachable stops being
	// retried ahead of untried ones, short enough that a node recovering
	// from a transient outage comes back.
	nodeFailureCooldown = 20 * time.Minute
)

// propNodes tracks lxmf.propagation announces (SPEC §5.8.5) and answers
// "which node(s) should this notification be left with?".
type propNodes struct {
	log       *log.Logger
	transport *rns.Transport
	// pinned is the operator's chosen node, or nil for auto-selection.
	pinned []byte
	now    func() time.Time

	mu    sync.Mutex
	nodes map[string]*propNode // dest_hash hex -> state
}

// propNode is one node's announced parameters plus our own experience
// of it.
type propNode struct {
	destHash   []byte
	firstHeard time.Time
	lastHeard  time.Time
	// usable is false when the announced parameters make the node
	// unusable (see acceptable). Recorded rather than discarded so the
	// operator log can say why a node is being skipped.
	usable bool
	// lastOK / lastFail are the outcomes of OUR uploads, which is
	// evidence of reachability that an announce cannot give.
	lastOK   time.Time
	lastFail time.Time
}

func newPropNodes(logger *log.Logger, transport *rns.Transport, pinned []byte) *propNodes {
	return &propNodes{
		log:       logger,
		transport: transport,
		pinned:    append([]byte(nil), pinned...),
		now:       time.Now,
		nodes:     make(map[string]*propNode),
	}
}

// AspectMatch implements rns.AnnounceHandler — only lxmf.propagation
// announces reach OnAnnounce.
func (p *propNodes) AspectMatch(nameHash []byte) bool {
	return bytes.Equal(nameHash, lxmfPropagationNameHash)
}

// OnAnnounce records a node from its §5.8.5 app_data. A node whose
// app_data will not parse is a node we cannot negotiate stamps or
// limits with, so it never becomes a candidate.
func (p *propNodes) OnAnnounce(a *rns.Announce) {
	if a == nil || len(a.DestHash) != rns.IdentityHashLen {
		return
	}
	info, err := lxmf.ParsePropagationNodeAppData(a.AppData)
	if err != nil {
		return
	}
	usable, reason := acceptable(info)
	key := hex.EncodeToString(a.DestHash)
	now := p.now()

	p.mu.Lock()
	prev := p.nodes[key]
	if prev == nil {
		p.evictLocked(now)
		p.nodes[key] = &propNode{
			destHash:   append([]byte(nil), a.DestHash...),
			firstHeard: now,
			lastHeard:  now,
			usable:     usable,
		}
	} else {
		prev.lastHeard = now
		prev.usable = usable
	}
	p.mu.Unlock()

	switch {
	case prev == nil && usable:
		p.log.Printf("lxmf: heard propagation node %s (stamp_cost=%d transfer_limit=%dKB)",
			key[:8], info.StampCost, info.PerTransferLimitKB)
	case prev == nil:
		p.log.Printf("lxmf: ignoring propagation node %s — %s", key[:8], reason)
	case prev.usable != usable && !usable:
		p.log.Printf("lxmf: propagation node %s no longer usable — %s", key[:8], reason)
	}
}

// acceptable decides at SELECTION time whether a node's announced
// parameters are ones we can satisfy.
//
// Deliberately not deferred to send time. Every value here is chosen by
// whoever sent the announce, and discovering a bad one inside
// SendPropagated means the upload has already been attempted and the
// notification has already burned a push interval against a node that
// was never going to work. Filtering here turns "lose thirty minutes"
// into "pick a different node".
func acceptable(info *lxmf.PropagationNodeInfo) (bool, string) {
	if info == nil {
		return false, "no announce parameters"
	}
	if !info.Enabled {
		return false, "not accepting messages"
	}
	if info.StampCost > lxmf.MaxPropagationStampCost {
		return false, fmt.Sprintf("demands stamp_cost %d above local limit %d",
			info.StampCost, lxmf.MaxPropagationStampCost)
	}
	if info.PerTransferLimitKB > 0 && info.PerTransferLimitKB < minTransferLimitKB {
		return false, fmt.Sprintf("per-transfer limit %dKB is implausibly small",
			info.PerTransferLimitKB)
	}
	return true, ""
}

// evictLocked prunes stale entries and, if still at capacity, the
// least-recently-heard node. Caller must hold p.mu.
func (p *propNodes) evictLocked(now time.Time) {
	for k, n := range p.nodes {
		if now.Sub(n.lastHeard) > nodeStaleAfter {
			delete(p.nodes, k)
		}
	}
	for len(p.nodes) >= maxTrackedNodes {
		var oldestKey string
		var oldestAt time.Time
		for k, n := range p.nodes {
			if oldestKey == "" || n.lastHeard.Before(oldestAt) {
				oldestKey, oldestAt = k, n.lastHeard
			}
		}
		if oldestKey == "" {
			return
		}
		delete(p.nodes, oldestKey)
	}
}

// RecordResult remembers how an upload to a node went, so the next
// selection can act on it.
func (p *propNodes) RecordResult(destHash []byte, err error) {
	key := hex.EncodeToString(destHash)
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.nodes[key]
	if n == nil {
		// A pinned node the hub has never heard announce still deserves
		// a record: it is the one it will keep trying.
		n = &propNode{destHash: append([]byte(nil), destHash...), firstHeard: now, lastHeard: now, usable: true}
		p.nodes[key] = n
	}
	if err == nil {
		n.lastOK = now
	} else {
		n.lastFail = now
	}
}

// Select returns up to `want` nodes to upload through, best first.
//
// A pinned node is returned alone and unconditionally: an operator who
// names one has made a trust decision, and quietly failing over to a
// stranger's node — or fanning out to several — would undo it.
func (p *propNodes) Select(want int) [][]byte {
	if len(p.pinned) > 0 {
		return [][]byte{append([]byte(nil), p.pinned...)}
	}
	if want < 1 {
		want = 1
	}
	now := p.now()

	p.mu.Lock()
	type candidate struct {
		hash []byte
		tier int
		age  time.Time
	}
	var candidates []candidate
	for _, n := range p.nodes {
		if !n.usable ||
			now.Sub(n.lastHeard) > nodeStaleAfter ||
			now.Sub(n.firstHeard) < nodeMinAge {
			continue
		}
		// The announce cache is the authority on the CURRENT parameters:
		// our own record can be a stale copy if the entry was evicted and
		// relearned, and SendPropagated reads the cache, not this map.
		known := p.transport.Recall(n.destHash)
		if known == nil {
			continue
		}
		info, err := lxmf.ParsePropagationNodeAppData(known.AppData)
		if err != nil {
			continue
		}
		if ok, _ := acceptable(info); !ok {
			continue
		}
		candidates = append(candidates, candidate{
			hash: append([]byte(nil), n.destHash...),
			tier: tierOf(n, now),
			age:  n.firstHeard,
		})
	}
	p.mu.Unlock()

	// Best tier first; within a tier, the node known longest.
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0; j-- {
			a, b := candidates[j-1], candidates[j]
			if a.tier < b.tier || (a.tier == b.tier && !b.age.Before(a.age)) {
				break
			}
			candidates[j-1], candidates[j] = b, a
		}
	}
	if len(candidates) > want {
		candidates = candidates[:want]
	}
	out := make([][]byte, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.hash)
	}
	return out
}

// tierOf ranks a node by our own experience of it. Lower is better.
//
//	0 — an upload has completed to it, and nothing has failed since.
//	1 — untried, or a failure old enough to have been forgiven.
//	2 — failed recently.
//
// A recent failure demotes rather than disqualifies: on a hub that has
// only ever talked to one node, "the node that failed us twenty minutes
// ago" is still a better answer than "nowhere".
func tierOf(n *propNode, now time.Time) int {
	if !n.lastOK.IsZero() && n.lastOK.After(n.lastFail) {
		return 0
	}
	if !n.lastFail.IsZero() && now.Sub(n.lastFail) < nodeFailureCooldown {
		return 2
	}
	return 1
}

// Compile-time guard: propNodes implements rns.AnnounceHandler.
var _ rns.AnnounceHandler = (*propNodes)(nil)
