package service

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/thatSFguy/reticulum-go/lxmf"
	"github.com/thatSFguy/reticulum-go/rns"
	"github.com/thatSFguy/reticulum-relay-chat/internal/hub"
	"github.com/thatSFguy/reticulum-relay-chat/internal/lxmfaddr"
)

// LXMF notification: reaching an RRC user whose link is gone.
//
// RRC has no store-and-forward, so a hub that wants to tell somebody
// something after they disconnect has to leave it somewhere they will
// look. LXMF propagation nodes are exactly that — a node holds a message
// until the recipient's own client syncs — and the recipient's client is
// one they already run, so nothing here asks anyone to install or update
// anything.
//
// The address comes free. A §6.6 LINKIDENTIFY carries the peer's full
// public key, and an RNS destination hash is a pure function of that key
// and a name, so the hub can compute where the peer's LXMF client
// listens without an announce, a lookup, or a registration step. See
// internal/lxmfaddr.
//
// The assumption is that the peer drives RRC from the same identity its
// LXMF client uses. That holds for reticulum-mobile-app, whose engine
// feeds one identity to both, but it is a property of a client rather
// than of the protocol — so a failure here is ordinary, and the hub
// keeps the mention queued for delivery over RRC instead.

// ErrNoPropagationNode is returned when no usable node is known yet.
var ErrNoPropagationNode = errors.New("no propagation node available")

// nodeMinAge is how long a freshly-heard node must have been known
// before auto-selection will use it. A node announced seconds ago has
// not proved anything; preferring one we have held for a while biases
// toward nodes that are actually stable.
const nodeMinAge = 2 * time.Minute

// lxmfNotifier implements hub.OfflineNotifier over LXMF propagation.
type lxmfNotifier struct {
	svc      *Service
	delivery *lxmf.Delivery

	// pinned is the operator's chosen node, or nil for auto-selection.
	pinned []byte

	mu sync.Mutex
	// nodes are the lxmf.propagation destinations heard announcing,
	// with when we first heard each. Auto-selection prefers the
	// longest-known node that is currently accepting.
	nodes map[string]time.Time
}

// newLXMFNotifier builds the notifier and registers the hub's own LXMF
// delivery destination.
//
// Registering a delivery destination for a hub that is not an inbox may
// look odd, but it is what makes the notification a real message rather
// than an anonymous one: the recipient sees a sender they can reply to,
// and their client can return a delivery proof.
func newLXMFNotifier(s *Service) (*lxmfNotifier, error) {
	delivery, err := lxmf.NewDelivery(s.transport, s.identity, s.buildDeliveryAnnounce)
	if err != nil {
		return nil, fmt.Errorf("lxmf delivery: %w", err)
	}
	n := &lxmfNotifier{
		svc:      s,
		delivery: delivery,
		nodes:    make(map[string]time.Time),
	}

	if hexHash := strings.TrimSpace(s.cfg.Hub.LXMFPropagationNode); hexHash != "" {
		pinned, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(hexHash), "0x"))
		if err != nil || len(pinned) != rns.IdentityHashLen {
			return nil, fmt.Errorf("lxmf_propagation_node must be %d hex bytes", rns.IdentityHashLen)
		}
		n.pinned = pinned
		s.log.Printf("lxmf: mention notifications via pinned propagation node %x", pinned)
	} else {
		s.log.Printf("lxmf: mention notifications via auto-selected propagation node")
	}

	// Learn nodes from announces. Even a pinned node benefits: the
	// upload needs the node's announced app_data to know whether it is
	// accepting and what it charges.
	s.transport.RegisterAnnounceHandler(n)
	return n, nil
}

// buildDeliveryAnnounce builds an announce for the hub's OWN
// lxmf.delivery destination.
//
// Without it the hub can emit a notification but nobody can accept one.
// The recipient sees a message whose source is a destination it has
// never heard announce and holds no public key for, so it cannot verify
// the signature — and a client configured to drop unverified messages
// discards it in silence. It also cannot reply or return a delivery
// proof, which is the entire reason this hub registers a delivery
// destination rather than sending anonymously.
//
// The app_data is the §4.3 form: display name, and a nil stamp_cost
// because this destination is an outbox, not an inbox — it has no
// inbound traffic to price and asking senders to grind proof-of-work at
// a hub that will not read their replies would be dishonest.
func (s *Service) buildDeliveryAnnounce(context byte) (*rns.Packet, error) {
	appData, err := rns.EncodeLXMFAppData([]byte(s.cfg.Hub.Name), nil)
	if err != nil {
		return nil, fmt.Errorf("lxmf announce app_data: %w", err)
	}
	return rns.BuildAnnounceWithContext(s.identity, lxmf.FullName(), appData, nil, context)
}

// announceDelivery broadcasts that announce. Called wherever the hub
// announces rrc.hub, so the two destinations keep the same schedule.
func (s *Service) announceDelivery() {
	if s.lxmfDest == nil {
		return
	}
	appData, err := rns.EncodeLXMFAppData([]byte(s.cfg.Hub.Name), nil)
	if err != nil {
		s.log.Printf("lxmf announce build failed: %v", err)
		return
	}
	pkt, err := rns.BuildAnnounce(s.identity, lxmf.FullName(), appData, nil)
	if err != nil {
		s.log.Printf("lxmf announce build failed: %v", err)
		return
	}
	if err := s.transport.Broadcast(pkt); err != nil {
		s.log.Printf("lxmf announce broadcast failed: %v", err)
		return
	}
	s.log.Printf("announced lxmf.delivery (%x)", s.lxmfDest)
}

// AspectMatch selects lxmf.propagation announces.
func (n *lxmfNotifier) AspectMatch(nameHash []byte) bool {
	want := rns.NameHash(lxmf.PropagationFullName())
	return len(nameHash) == len(want) && equalBytes(nameHash, want)
}

// OnAnnounce records a propagation node we have heard from.
func (n *lxmfNotifier) OnAnnounce(a *rns.Announce) {
	if a == nil || len(a.DestHash) != rns.IdentityHashLen {
		return
	}
	key := hex.EncodeToString(a.DestHash)
	n.mu.Lock()
	if _, known := n.nodes[key]; !known {
		n.nodes[key] = time.Now()
		n.svc.log.Printf("lxmf: heard propagation node %s", key[:8])
	}
	n.mu.Unlock()
}

// selectNode returns the propagation node to upload through.
//
// A pinned node is used unconditionally — an operator who names one has
// made a trust decision, and quietly failing over to a stranger's node
// would undo it. Otherwise the longest-known node that is currently
// accepting wins: uptime is the only evidence available without probing.
func (n *lxmfNotifier) selectNode() ([]byte, error) {
	if n.pinned != nil {
		return n.pinned, nil
	}

	n.mu.Lock()
	type candidate struct {
		hash  []byte
		since time.Time
	}
	var candidates []candidate
	for key, since := range n.nodes {
		h, err := hex.DecodeString(key)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{hash: h, since: since})
	}
	n.mu.Unlock()

	var best []byte
	var bestSince time.Time
	now := time.Now()
	for _, c := range candidates {
		if now.Sub(c.since) < nodeMinAge {
			continue
		}
		// The announce cache carries the node's app_data; a node that is
		// not accepting is not a candidate however long we have known it.
		known := n.svc.transport.Recall(c.hash)
		if known == nil {
			continue
		}
		info, err := lxmf.ParsePropagationNodeAppData(known.AppData)
		if err != nil || !info.Enabled {
			continue
		}
		if best == nil || c.since.Before(bestSince) {
			best, bestSince = c.hash, c.since
		}
	}
	if best == nil {
		return nil, ErrNoPropagationNode
	}
	return best, nil
}

// ensureAddressable makes sure the transport can encrypt to known's
// destination, without discarding anything better it already holds.
//
// Restore overwrites a cache entry wholesale, and the entry synthesized
// from a LINKIDENTIFY key carries a public key and nothing else — no
// app_data, no hop count, no last-seen. app_data is the part that
// matters: it carries the recipient's §5.7.4 stamp_cost, and
// SendPropagated grinds a delivery stamp only when it can read one.
// Overwriting a real announce with the stub drops that cost to zero and
// sends an unstamped message, which a recipient enforcing stamps
// discards silently — the hub logs a successful upload and nothing ever
// arrives.
//
// A live announce therefore always wins. It is strictly richer, and it
// is the same key: Restore re-derives the destination from the public
// key before accepting an entry, so a different key could not have been
// cached under this destination in the first place. The synthesized
// entry fills in only for a peer the hub has never heard announce,
// which is exactly the case this path exists for.
func ensureAddressable(t *rns.Transport, known *rns.KnownIdentity) {
	if t == nil || known == nil {
		return
	}
	if t.Recall(known.DestHash) != nil {
		return
	}
	t.Restore(known)
}

// PinPeers implements hub.PeerAddressPinner: it keeps the announce-cache
// entries of the hub's known peers resident, so their §5.7.4 stamp_cost
// survives the churn of a busy public mesh.
func (n *lxmfNotifier) PinPeers(pubKeys [][]byte) {
	dests := make([][]byte, 0, len(pubKeys))
	for _, pub := range pubKeys {
		dest, err := lxmfaddr.DeliveryDest(pub)
		if err != nil {
			continue
		}
		dests = append(dests, dest)
	}
	n.svc.transport.PinDestinations(dests)
}

// NotifyAbsent uploads one notification for the peer owning pubKey to a
// propagation node, where it waits for that peer's client to sync.
func (n *lxmfNotifier) NotifyAbsent(pubKey []byte, title, body string) error {
	known, err := lxmfaddr.KnownDelivery(pubKey)
	if err != nil {
		return err
	}
	// Teach the transport how to encrypt to this peer, but only if it
	// does not already know it. Restore overwrites a cache entry
	// wholesale, and the entry we synthesize here carries a public key
	// and nothing else — no app_data, no hop count, no last-seen.
	//
	// app_data is the part that matters. It carries the recipient's
	// §5.7.4 stamp_cost, and SendPropagated grinds a delivery stamp
	// only when it can read one. Clobbering a real announce with this
	// stub therefore drops the cost to zero and sends an unstamped
	// message, which a recipient that enforces stamps discards without
	// a word — the hub logs a successful upload and the notification
	// never arrives.
	//
	// So a live announce always wins: it is strictly richer, and it is
	// the same key (Restore re-derives the destination from the public
	// key before accepting it, so a mismatch could not have been
	// cached under this destination anyway). We fill in only for a
	// peer the hub has never heard announce, which is precisely the
	// case this whole path exists for.
	ensureAddressable(n.svc.transport, known)

	node, err := n.selectNode()
	if err != nil {
		return err
	}
	// Whether we can stamp depends on having heard this peer announce:
	// the stamp_cost lives in that announce's app_data, and nothing
	// else carries it. Decide before sending, because ensureAddressable
	// may be about to install a stub that has none.
	stamped := false
	if cached := n.svc.transport.Recall(known.DestHash); cached != nil && len(cached.AppData) > 0 {
		stamped = true
	}

	if _, err := n.delivery.SendPropagated(node, known.DestHash, []byte(title), []byte(body), nil); err != nil {
		return fmt.Errorf("propagate to %x via %x: %w", known.DestHash[:4], node[:4], err)
	}
	if !stamped {
		// The upload succeeded; its acceptance did not. See
		// hub.ErrDeliveredUnstamped.
		return fmt.Errorf("%w: %x has never announced", hub.ErrDeliveredUnstamped, known.DestHash[:4])
	}
	return nil
}
