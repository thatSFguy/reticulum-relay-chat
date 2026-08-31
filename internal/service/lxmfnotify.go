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
	// startedAt bounds how long a missing announce is forgiven as a
	// cold cache rather than treated as a fault. See waitingCondition.
	startedAt time.Time
	now       func() time.Time
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
	if s.lxmfIdentity == nil {
		return nil, errors.New("no lxmf notification identity")
	}
	delivery, err := lxmf.NewDelivery(s.transport, s.lxmfIdentity, s.buildDeliveryAnnounce)
	if err != nil {
		return nil, fmt.Errorf("lxmf delivery: %w", err)
	}
	delivery.DeliveryProofTimeout = notifyProofTimeout
	delivery.LinkSendTimeout = notifyLinkSendTimeout
	delivery.OnError = func(err error) { s.log.Printf("lxmf: %v", err) }
	delivery.OnMessage = s.answerInboundLXMF

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
		svc:       s,
		delivery:  delivery,
		nodes:     newPropNodes(s.log, s.transport, pinned),
		fanout:    s.cfg.Hub.LXMFPropagationFanout,
		startedAt: time.Now(),
		now:       time.Now,
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
//
// It is signed by s.lxmfIdentity, not the hub identity. The two
// destinations used to share one key, which left a client free to store
// them as a single entry keyed by identity and let the second announce
// overwrite the first — see Config.LXMFIdentityPath.
//
// The name is cfg.LXMFName(), NOT cfg.Name. A messaging client lists an
// lxmf.delivery announce beside real people, so under the hub's own
// name this destination is indistinguishable from somebody you can talk
// to — and the hub would appear twice in an announce list, once as a
// room to join and once as a correspondent. Only the first is true.
func (s *Service) buildDeliveryAnnounce(context byte) (*rns.Packet, error) {
	appData, err := rns.EncodeLXMFAppData([]byte(s.cfg.Hub.LXMFName()), nil)
	if err != nil {
		return nil, fmt.Errorf("lxmf announce app_data: %w", err)
	}
	return rns.BuildAnnounceWithContext(s.lxmfIdentity, lxmf.FullName(), appData, nil, context)
}

// announceDelivery broadcasts that announce. Called wherever the hub
// announces rrc.hub, so the two destinations keep the same schedule.
func (s *Service) announceDelivery() {
	if s.lxmfDest == nil {
		return
	}
	// Built through buildDeliveryAnnounce so the announce this
	// broadcasts and the one the Transport builds for a path response
	// cannot name different identities. They were two call sites, and
	// the identity is now the thing this file exists to keep separate.
	pkt, err := s.buildDeliveryAnnounce(rns.ContextNone)
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

	// Whether we can meet this peer's §5.7.4 stamp demand depends on
	// having heard them announce: the cost lives in that announce's
	// app_data and nothing else carries it. Note this is "we know their
	// policy", not "we ground a stamp" — most peers announce a cost of
	// zero, and knowing that is knowing we have satisfied it. Decided
	// before sending, because ensureAddressable may just have installed
	// a stub that carries no app_data at all.
	knowStampPolicy := false
	if cached := n.svc.transport.Recall(known.DestHash); cached != nil && len(cached.AppData) > 0 {
		knowStampPolicy = true
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
		if !knowStampPolicy {
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
	// allWaiting stays true while every failure so far is a "not yet"
	// rather than a "no": see waitingCondition.
	allWaiting := true
	for _, node := range nodes {
		_, err := n.delivery.SendPropagated(node, known.DestHash, []byte(title), []byte(body), nil)
		n.nodes.RecordResult(node, err)
		if err != nil {
			lastErr = err
			allWaiting = allWaiting && waitingCondition(err)
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
		// The FULL node hash, not a prefix. Store-and-forward only
		// completes when the recipient fetches, so "which node is
		// holding this?" is the first question when a notification is
		// accepted here and never arrives — and answering it means
		// going to that node, which a four-byte prefix cannot address.
		n.svc.log.Printf("lxmf: uploaded notification for %x to node %x",
			known.DestHash[:4], node)
	}
	if uploaded == 0 {
		// Judged on the PROPAGATION route alone. The direct route
		// failing is not evidence of anything wrong — an absent peer is
		// exactly who this fallback exists for, and their delivery
		// proof timing out is the normal way of discovering it.
		if allWaiting && n.warmingUp() {
			return fmt.Errorf("%w: %v (direct: %v)",
				hub.ErrNotifierUnavailable, lastErr, directErr)
		}
		return fmt.Errorf("no route to %x: direct: %v; propagation: %w",
			known.DestHash[:4], directErr, lastErr)
	}
	// The upload succeeded; its ACCEPTANCE did not. A node acknowledges
	// storage, never receipt, so the mention stays queued for RRC
	// whatever the stamp situation — the fallback is not something to
	// spend on an acknowledgement that does not mean what we want it to
	// mean. See hub.ErrDeliveredUnconfirmed.
	if !knowStampPolicy {
		return fmt.Errorf("%w: %x has never announced", hub.ErrDeliveredUnconfirmed, known.DestHash[:4])
	}
	return fmt.Errorf("%w: uploaded to %d node(s), no receipt from the recipient",
		hub.ErrDeliveredUnconfirmed, uploaded)
}

// notifierWarmup is how long after start a missing announce is read as
// a cold cache rather than a fault.
//
// The announce cache is in-memory only (there is no announces.json), so
// a hub that has just restarted knows nothing about anybody — including
// a propagation node its own operator pinned. Long enough to cover that
// gap; short enough that a node which is genuinely gone stops being
// retried every prune tick and falls back to the ordinary push
// interval, instead of spending the mesh's airtime on a
// misconfiguration forever.
const notifierWarmup = 5 * time.Minute

func (n *lxmfNotifier) warmingUp() bool {
	return n.now().Sub(n.startedAt) < notifierWarmup
}

// waitingCondition reports an error that means "not yet" rather than
// "no": the announce we need has not arrived, so there is no key to
// encrypt with and no path to open a link over. Both send paths ask the
// network for the missing path before returning, which makes the next
// attempt materially more likely to work.
//
// Observed live, and the reason this exists: a pinned node failed with
// ErrPropagationNodeUnknown 40 seconds after start, the path request
// that failure triggered was answered 1.5 seconds later — and the
// mention was parked for half an hour anyway, because the hub had
// already spent the peer's push interval on it.
func waitingCondition(err error) bool {
	return errors.Is(err, lxmf.ErrPropagationNodeUnknown) ||
		errors.Is(err, lxmf.ErrRecipientUnknown)
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

// DiagnoseRoute describes, without sending anything, how this notifier
// would reach a peer. It implements hub.NotifyDiagnoser.
//
// Everything here is already in the hub's log at the moment a
// notification fails. The point of restating it is that the person it
// failed to reach cannot read that log, and the two facts they most
// need — the address the hub derived for them, and whether the hub has
// ever heard that address announce — are the two that decide whether
// the problem is on the hub's side or in the identity their messaging
// client uses.
func (n *lxmfNotifier) DiagnoseRoute(pubKey []byte) hub.NotifyRoute {
	dest, err := lxmfaddr.DeliveryDest(pubKey)
	if err != nil {
		return hub.NotifyRoute{Address: "(cannot derive an address from your key: " + err.Error() + ")"}
	}
	route := hub.NotifyRoute{Address: fmt.Sprintf("%x", dest)}

	// Announced is read the same way NotifyAbsent reads it: a cached
	// announce with app_data is what carries the §5.7.4 stamp_cost, and
	// nothing else does.
	if cached := n.svc.transport.Recall(dest); cached != nil && len(cached.AppData) > 0 {
		route.Announced = true
	} else {
		route.Notes = append(route.Notes,
			"the hub has not heard that address announce, so it cannot meet a stamp requirement if you set one")
	}

	// The store-and-forward fallback, which only matters when the
	// direct route does not answer.
	if nodes := n.nodes.Select(n.fanout); len(nodes) > 0 {
		route.Notes = append(route.Notes,
			fmt.Sprintf("fallback: %d store-and-forward node(s) usable — but a node holds a message until YOUR client syncs from it, and no announce says which node that is", len(nodes)))
	} else if n.warmingUp() {
		route.Notes = append(route.Notes,
			"fallback: no store-and-forward node known yet — the hub started recently and is still learning the mesh")
	} else {
		route.Notes = append(route.Notes,
			"fallback: no store-and-forward node usable, so a notification only lands if you are directly reachable")
	}
	return route
}

// NotifyDirect delivers over route 1 only, with no store-and-forward
// fallback. It implements hub.DirectNotifier.
//
// The stamp caveat is the same one NotifyAbsent applies, and for the
// same reason: a client that enforces §5.7.4 stamps still proofs the
// RNS packet before dropping the LXMF body, so a proof from a peer we
// have never heard announce is not evidence the message survived.
// Reporting that as success would be exactly the lie this command
// exists to expose.
func (n *lxmfNotifier) NotifyDirect(pubKey []byte, title, body string) error {
	known, err := lxmfaddr.KnownDelivery(pubKey)
	if err != nil {
		return err
	}
	ensureAddressable(n.svc.transport, known)

	knowStampPolicy := false
	if cached := n.svc.transport.Recall(known.DestHash); cached != nil && len(cached.AppData) > 0 {
		knowStampPolicy = true
	}

	if err := n.sendDirect(known.DestHash, title, body); err != nil {
		n.svc.log.Printf("lxmf: direct-only delivery to %x failed: %v", known.DestHash[:4], err)
		return err
	}
	n.svc.log.Printf("lxmf: delivered to %x directly (proof received)", known.DestHash[:4])
	if !knowStampPolicy {
		return fmt.Errorf("%w: %x has never announced", hub.ErrDeliveredUnconfirmed, known.DestHash[:4])
	}
	return nil
}

// inboundReplyInterval throttles the auto-reply per sender.
//
// The reply is itself an LXMF send, so an unthrottled one turns any
// inbound message into a round trip somebody else could drive — and two
// hubs pointed at each other into a loop. One answer per sender per
// hour is enough to be helpful and too little to be a weapon.
const inboundReplyInterval = time.Hour

// answerInboundLXMF replies to somebody who messaged the hub's
// notification address.
//
// That address is an outbox: the hub sends mention notifications from
// it and reads nothing. But it announces on lxmf.delivery, which is what
// makes it verifiable — and which also makes every messaging client
// list it as a contact. Somebody will tap it and say hello. Before
// this, that message went nowhere and they got silence, which looks
// exactly like a hub that is broken.
//
// So the hub answers once, says what the address is, and points at the
// thing they actually wanted: the room, in a form they can act on.
func (s *Service) answerInboundLXMF(msg *lxmf.Message) {
	if msg == nil || len(msg.SourceHash) == 0 {
		return
	}
	// Logged on arrival, not only on a successful answer. Somebody
	// messaging the notification address is a thing an operator wants
	// to know happened, and without this line an inbound message that
	// is throttled, or that arrives before the notifier is installed,
	// is indistinguishable from one that never arrived at all.
	s.log.Printf("lxmf: inbound message from %x to the notifications-only address (%q)",
		msg.SourceHash[:4], snippetOf(msg.Title, msg.Content))

	key := hex.EncodeToString(msg.SourceHash)

	s.mu.Lock()
	if s.lastInboundReply == nil {
		s.lastInboundReply = make(map[string]time.Time)
	}
	now := time.Now()
	last, seen := s.lastInboundReply[key]
	if seen && now.Sub(last) < inboundReplyInterval {
		s.mu.Unlock()
		return
	}
	s.lastInboundReply[key] = now
	// Bounded like every other per-peer map here: sweep what can no
	// longer suppress anything.
	for k, t := range s.lastInboundReply {
		if now.Sub(t) >= inboundReplyInterval {
			delete(s.lastInboundReply, k)
		}
	}
	s.mu.Unlock()

	n := s.notifier
	if n == nil {
		return
	}
	name := s.cfg.Hub.Name
	if name == "" {
		name = "this hub"
	}
	title := name + ": notifications only"
	body := "This address only sends notifications — nobody reads replies here.\n\n" +
		name + " is an RRC chat hub. To join it, add this hub in an RRC client:\n" +
		s.hubLinkForInvite() + "\n\n" +
		"You are getting this because you messaged the address the hub sends " +
		"mention notifications from."

	// sendDirect, not NotifyDirect: a reply already HAS the recipient's
	// destination hash, straight off the message we just verified.
	// NotifyDirect takes a 64-byte public key and derives the
	// destination from it — that is the shape the mention path has,
	// where all the hub retains of an absent peer is their key.
	//
	// Off the dispatcher goroutine either way: a send blocks for a proof.
	go func() {
		if err := n.sendDirect(msg.SourceHash, title, body); err != nil {
			s.log.Printf("lxmf: could not answer inbound from %x: %v", msg.SourceHash[:4], err)
			return
		}
		s.log.Printf("lxmf: answered inbound message from %x (notifications-only address)",
			msg.SourceHash[:4])
	}()
}

// snippetOf renders a short, bounded preview of an inbound message for
// the log. Attacker-supplied text: truncated, and newlines flattened so
// one message cannot forge extra log lines.
func snippetOf(title, content []byte) string {
	t := strings.TrimSpace(string(title) + " " + string(content))
	t = strings.NewReplacer("\n", " ", "\r", " ").Replace(t)
	const max = 48
	if len(t) > max {
		r := []rune(t)
		if len(r) > max {
			r = r[:max]
		}
		return string(r) + "…"
	}
	return t
}

// hubLinkForInvite renders the hub's own room link, or its bare
// destination hash when the link format cannot be built.
func (s *Service) hubLinkForInvite() string {
	return "rrc@" + hex.EncodeToString(s.destHash)
}
