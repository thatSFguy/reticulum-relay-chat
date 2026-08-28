package service

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
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
// look. Their LXMF client is that somewhere, and it is one they already
// run — nothing here asks anyone to install or update anything.
//
// The address comes free. A §6.7.6 LINKIDENTIFY carries the peer's full
// public key, and an RNS destination hash is a pure function of that key
// and a name, so the hub can compute where the peer's LXMF client
// listens without an announce, a lookup, or a registration step. See
// internal/lxmfaddr.
//
// TWO ROUTES, IN THIS ORDER.
//
// 1. Direct. The message goes to the peer's lxmf.delivery destination
//    and the call blocks for the recipient's own §6.5 delivery proof.
//
// 2. Propagation. The message is left with a store-and-forward node,
//    which holds it until the peer's client next syncs.
//
// Order matters more than it looks. "Not connected to the room" is not
// "off the mesh": an RRC client is a foreground app somebody closes,
// while their LXMF client is a background service on the same device
// that keeps announcing. In the live test that motivated this, the
// recipient's lxmf.delivery destination announced at ONE HOP throughout
// the entire session — the phone was right there — and the hub sent to
// a propagation node anyway, because propagation was the only route it
// had. Nothing arrived.
//
// The routes also differ in what success means, and the difference is
// the whole reason this file is careful. A propagation upload is
// acknowledged by the NODE; it says nothing about the recipient, who
// may never sync from that node (see propnodes.go). A direct send is
// acknowledged by the RECIPIENT'S OWN STACK. It is the only signal on
// either route that means the message actually landed, which is why it
// is tried first and why only it clears the mention outright.

// ErrNoPropagationNode is returned when no usable node is known yet.
var ErrNoPropagationNode = errors.New("no propagation node available")

// notifyDirectTimeouts bound one direct attempt. Both are shorter than
// the library defaults (15s / 30s) because this runs on the hub's prune
// loop, serially over every absent peer with something waiting: a hub
// with a dozen departed members must not spend six minutes there. An
// unreachable peer is the expected case, and failing fast just moves on
// to the propagation route.
const (
	notifyProofTimeout    = 10 * time.Second
	notifyLinkSendTimeout = 20 * time.Second
)

// lxmfNotifier implements hub.OfflineNotifier over LXMF.
type lxmfNotifier struct {
	svc      *Service
	delivery *lxmf.Delivery
	nodes    *propNodes
	// fanout is how many propagation nodes one notification is left
	// with when no node is pinned. See Config.LXMFPropagationFanout.
	fanout int
}

// newLXMFNotifier builds the notifier and registers the hub's own LXMF
// delivery destination.
//
// Registering a delivery destination for a hub that is not an inbox may
// look odd, but it is what makes the notification a real message rather
// than an anonymous one: the recipient sees a sender they can reply to,
// and their client can return the delivery proof the direct route
// depends on.
func newLXMFNotifier(s *Service) (*lxmfNotifier, error) {
	delivery, err := lxmf.NewDelivery(s.transport, s.identity, s.buildDeliveryAnnounce)
	if err != nil {
		return nil, fmt.Errorf("lxmf delivery: %w", err)
	}
	delivery.DeliveryProofTimeout = notifyProofTimeout
	delivery.LinkSendTimeout = notifyLinkSendTimeout
	delivery.OnError = func(err error) { s.log.Printf("lxmf: %v", err) }

	var pinned []byte
	if hexHash := strings.TrimSpace(s.cfg.Hub.LXMFPropagationNode); hexHash != "" {
		pinned, err = hex.DecodeString(strings.TrimPrefix(strings.ToLower(hexHash), "0x"))
		if err != nil || len(pinned) != rns.IdentityHashLen {
			return nil, fmt.Errorf("lxmf_propagation_node must be %d hex bytes", rns.IdentityHashLen)
		}
		s.log.Printf("lxmf: notifications direct, falling back to pinned propagation node %x", pinned)
	} else {
		s.log.Printf("lxmf: notifications direct, falling back to %d auto-selected propagation node(s)",
			s.cfg.Hub.LXMFPropagationFanout)
	}

	n := &lxmfNotifier{
		svc:      s,
		delivery: delivery,
		nodes:    newPropNodes(s.log, s.transport, pinned),
		fanout:   s.cfg.Hub.LXMFPropagationFanout,
	}
	// Learn nodes from announces. Even a pinned node benefits: the
	// upload needs the node's announced app_data to know whether it is
	// accepting and what it charges.
	s.transport.RegisterAnnounceHandler(n.nodes)
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
// proof, which the direct route blocks on.
//
// The app_data is the §4.3 form: display name, and a nil stamp_cost
// because this destination is an outbox, not an inbox — it has no
// inbound traffic to price, and asking senders to grind proof-of-work
// at a hub that will not read their replies would be dishonest.
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

// ensureAddressable makes sure the transport can encrypt to known's
// destination, without discarding anything better it already holds.
//
// Restore overwrites a cache entry wholesale, and the entry synthesized
// from a LINKIDENTIFY key carries a public key and nothing else — no
// app_data, no hop count, no last-seen. app_data is the part that
// matters: it carries the recipient's §5.7.4 stamp_cost, and both send
// routes grind a delivery stamp only when they can read one.
// Overwriting a real announce with the stub drops that cost to zero and
// sends an unstamped message, which a recipient enforcing stamps
// discards silently — the hub logs a success and nothing ever arrives.
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

// NotifyAbsent delivers one notification to the peer owning pubKey:
// straight to their LXMF client if it answers, and otherwise into
// store-and-forward to wait for them.
func (n *lxmfNotifier) NotifyAbsent(pubKey []byte, title, body string) error {
	known, err := lxmfaddr.KnownDelivery(pubKey)
	if err != nil {
		return err
	}
	ensureAddressable(n.svc.transport, known)

	// Whether we can stamp depends on having heard this peer announce:
	// the §5.7.4 stamp_cost lives in that announce's app_data and
	// nothing else carries it. Decide before sending, because
	// ensureAddressable may just have installed a stub that has none.
	stamped := false
	if cached := n.svc.transport.Recall(known.DestHash); cached != nil && len(cached.AppData) > 0 {
		stamped = true
	}

	// Route 1: direct. Blocks for the recipient's own delivery proof, so
	// success here is end-to-end evidence and not an intermediary's
	// opinion — the mention is genuinely delivered and the hub can drop
	// it. The stamp caveat below does not apply: a client that enforces
	// stamps still proofs the RNS packet before dropping the LXMF body,
	// so an unstamped direct send can be acknowledged and then
	// discarded, exactly as an unstamped upload can.
	directErr := n.sendDirect(known.DestHash, title, body)
	if directErr == nil {
		n.svc.log.Printf("lxmf: delivered to %x directly (proof received)", known.DestHash[:4])
		if !stamped {
			return fmt.Errorf("%w: %x has never announced", hub.ErrDeliveredUnconfirmed, known.DestHash[:4])
		}
		return nil
	}
	n.svc.log.Printf("lxmf: direct delivery to %x failed (%v) — trying store-and-forward",
		known.DestHash[:4], directErr)

	// Route 2: store-and-forward. See propnodes.go for why this is
	// several nodes rather than one.
	nodes := n.nodes.Select(n.fanout)
	if len(nodes) == 0 {
		// Neither route could be attempted. Say so distinctly: the hub
		// releases its per-peer throttle for this case instead of
		// spending half an hour on an attempt that never happened.
		return fmt.Errorf("%w: %v (direct also failed: %v)",
			hub.ErrNotifierUnavailable, ErrNoPropagationNode, directErr)
	}
	var uploaded int
	var lastErr error
	for _, node := range nodes {
		_, err := n.delivery.SendPropagated(node, known.DestHash, []byte(title), []byte(body), nil)
		n.nodes.RecordResult(node, err)
		if err != nil {
			lastErr = err
			n.svc.log.Printf("lxmf: upload for %x to node %x failed: %v",
				known.DestHash[:4], node[:4], err)
			// A pinned node we have never heard from cannot be linked
			// to — ask the network for its announce so a later attempt
			// can succeed. (Auto-discovered nodes can't hit this:
			// discovery IS an announce.)
			if errors.Is(err, lxmf.ErrPropagationNodeUnknown) {
				if rerr := n.svc.transport.RequestPath(node); rerr != nil {
					n.svc.log.Printf("lxmf: path? for node %x: %v", node[:4], rerr)
				}
			}
			continue
		}
		uploaded++
		n.svc.log.Printf("lxmf: uploaded notification for %x to node %x",
			known.DestHash[:4], node[:4])
	}
	if uploaded == 0 {
		return fmt.Errorf("no route to %x: direct: %v; propagation: %w",
			known.DestHash[:4], directErr, lastErr)
	}
	// The upload succeeded; its ACCEPTANCE did not. A node acknowledges
	// storage, never receipt, so the mention stays queued for RRC
	// whatever the stamp situation — the fallback is not something to
	// spend on an acknowledgement that does not mean what we want it to
	// mean. See hub.ErrDeliveredUnconfirmed.
	if !stamped {
		return fmt.Errorf("%w: %x has never announced", hub.ErrDeliveredUnconfirmed, known.DestHash[:4])
	}
	return fmt.Errorf("%w: uploaded to %d node(s), no receipt from the recipient",
		hub.ErrDeliveredUnconfirmed, uploaded)
}

// sendDirect attempts route 1. The library picks a single opportunistic
// packet or a Link by size, and blocks either way for the proof.
func (n *lxmfNotifier) sendDirect(dest []byte, title, body string) error {
	_, err := n.delivery.SendWithID(dest, []byte(title), []byte(body), nil)
	if err == nil {
		return nil
	}
	// An unknown recipient means the transport holds no route yet.
	// ensureAddressable installed the KEY, which is what encryption
	// needs, but a path is what routing needs — ask for one so the next
	// attempt, thirty minutes from now, has somewhere to send.
	if errors.Is(err, lxmf.ErrRecipientUnknown) || errors.Is(err, lxmf.ErrDeliveryProofTimeout) {
		if rerr := n.svc.transport.RequestPath(dest); rerr != nil {
			n.svc.log.Printf("lxmf: path? for %x: %v", dest[:4], rerr)
		}
	}
	return err
}
