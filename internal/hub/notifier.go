package hub

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thatSFguy/reticulum-relay-chat/internal/peerreg"
)

// Reaching someone who is not connected.
//
// Holding a mention until its recipient next opens the hub is better
// than dropping it, but it is not a notification: nothing tells them to
// look. The only thing in Reticulum that can reach a disconnected peer
// is LXMF store-and-forward — a message left with a propagation node,
// which their own client collects on its next sync and surfaces the way
// it surfaces any other message.
//
// The hub does not do that itself. It is transport-agnostic by design,
// and stays that way: it knows only that something might be able to
// reach an absent peer given their public key. internal/service
// implements that against LXMF.

// ErrDeliveredUnconfirmed reports a notification that went out but
// carries no evidence the recipient received it. The notifier did its
// job; what is missing is a receipt.
//
// Two things produce it. A store-and-forward upload is acknowledged by
// the NODE, never by the recipient — who may not sync from that node at
// all. And a message the hub could not proof-of-work stamp, because it
// has never heard the recipient announce and so does not know their
// §5.7.4 stamp_cost, is deliverable to a recipient that ignores stamps
// and discarded in silence by one that enforces them — and nothing
// tells the hub which it is dealing with.
//
// Either way the mention stays queued, so the RRC path remains the
// fallback: at worst the peer sees it twice, which is a far better
// failure than the notification vanishing. A mention is dropped only
// against a receipt from the recipient's own stack, which today means
// a successful direct send.
var ErrDeliveredUnconfirmed = errors.New("hub: notification sent without a receipt")

// ErrNotifierUnavailable reports that the notifier had no route to even
// attempt — not that an attempt failed.
//
// The distinction is worth a sentinel because the hub throttles pushes
// per peer at mentionPushInterval, and it sets that throttle BEFORE
// trying. Without this, a notifier that could not lift a finger — no
// propagation node discovered yet, the transport still coming up —
// spends the peer's next half hour anyway. That is the normal state of
// a hub for the first couple of minutes after it starts, which is
// exactly when a mention seeded from disk gets its first attempt.
var ErrNotifierUnavailable = errors.New("hub: notifier had no route to attempt")

// OfflineNotifier delivers a message to a peer the hub cannot reach over
// RRC, addressed by the public key that peer proved when it last
// identified.
//
// Implementations may block for seconds — a propagation-node upload is a
// link handshake, a transfer and a proof — and are called from the hub's
// background loop, never from the path that relays a message.
//
// Failure is ordinary, not exceptional: the recipient may use a
// different identity for LXMF, no propagation node may be reachable, or
// the node may refuse. The hub keeps the mention queued for RRC delivery
// whenever this returns an error.
type OfflineNotifier interface {
	NotifyAbsent(pubKey []byte, title, body string) error
}

// PeerAddressPinner is an optional OfflineNotifier capability: the hub
// tells it which peers must stay addressable, and it keeps their
// announce-cache entries from being evicted.
//
// This matters because the recipient's stamp_cost lives in that cached
// announce and nowhere else. The cache is bounded (reticulum-go's
// KnownIdentityCapacity, 4096) and evicts the oldest unpinned entry, so
// on a busy public mesh — where announcing peers outnumber slots
// several times over — the people this hub actually serves are pushed
// out by strangers it will never message. The mention would then go out
// unstamped and be discarded by a recipient that enforces stamps.
//
// The set is re-asserted whole rather than mutated, so a missed removal
// cannot leak a pin.
type PeerAddressPinner interface {
	PinPeers(pubKeys [][]byte)
}

// SetOfflineNotifier installs the notifier. Call before Start.
func (h *Hub) SetOfflineNotifier(n OfflineNotifier) {
	h.mu.Lock()
	h.notifier = n
	h.mu.Unlock()
	h.pinPeerAddresses()
}

// pinPeerAddresses re-asserts the set of peers that must stay
// addressable. Cheap and idempotent, so it runs whenever the directory
// changes rather than trying to track deltas.
func (h *Hub) pinPeerAddresses() {
	h.mu.Lock()
	pinner, _ := h.notifier.(PeerAddressPinner)
	var keys [][]byte
	if pinner != nil {
		keys = make([][]byte, 0, len(h.peers))
		for _, id := range peerreg.SortedIdentities(h.peers) {
			if p := h.peers[id]; len(p.PublicKey) == peerreg.PublicKeyLen {
				keys = append(keys, append([]byte(nil), p.PublicKey...))
			}
		}
	}
	h.mu.Unlock()
	if pinner != nil {
		pinner.PinPeers(keys)
	}
}

// mentionPushInterval is the minimum gap between push attempts for one
// peer. A propagation upload is expensive and a peer who is away stays
// away, so retrying every prune tick would spend the hub's airtime
// re-sending what a node already holds.
const mentionPushInterval = 30 * time.Minute

// pendingPush is one peer's outstanding notification.
type pendingPush struct {
	idHex  string
	pubKey []byte
	count  int
	// highSeq is the Seq of the newest mention in this push. On success
	// everything at or below it is dropped, which is exactly what was
	// sent — see the removal in pushPendingMentions.
	highSeq uint64
	title   string
	body    string
}

// pushPendingMentions hands each absent peer's waiting mentions to the
// notifier. Runs on the hub's prune loop, off the message path.
//
// Mentions that reach the notifier are dropped from the queue, because
// the peer will now hear about them through their LXMF client and a
// second copy on their next RRC session would be a duplicate. Mentions
// that fail stay queued, so the RRC path remains the fallback.
func (h *Hub) pushPendingMentions() {
	h.mu.Lock()
	n := h.notifier
	if n == nil || !h.cfg.MentionNotify {
		h.mu.Unlock()
		return
	}
	now := time.Now()
	var batch []pendingPush
	for _, idHex := range peerreg.SortedIdentities(h.peers) {
		p := h.peers[idHex]
		if len(p.Mentions) == 0 || p.NotifyOptOut || len(p.PublicKey) == 0 {
			continue
		}
		// Still connected: they will be told over RRC, and an LXMF copy
		// would arrive alongside it.
		if h.sessionForHashLocked(idHex) != nil {
			continue
		}
		if last, ok := h.lastPush[idHex]; ok && now.Sub(last) < mentionPushInterval {
			continue
		}
		h.lastPush[idHex] = now
		title, body := renderMentionNotification(h.cfg.Name, p.Mentions)
		batch = append(batch, pendingPush{
			idHex:   idHex,
			pubKey:  append([]byte(nil), p.PublicKey...),
			count:   len(p.Mentions),
			highSeq: p.Mentions[len(p.Mentions)-1].Seq,
			title:   title,
			body:    body,
		})
	}
	h.sweepLastPushLocked(now)
	h.mu.Unlock()

	for _, push := range batch {
		err := n.NotifyAbsent(push.pubKey, push.title, push.body)
		if errors.Is(err, ErrDeliveredUnconfirmed) {
			// Sent, but nothing proves it was received. Keep it queued
			// so the RRC path remains a fallback.
			h.log.Printf("mentions: sent %d notification(s) to %s… but got no receipt (%v); keeping them queued for RRC",
				push.count, push.idHex[:8], err)
			continue
		}
		if errors.Is(err, ErrNotifierUnavailable) {
			// Nothing was attempted, so nothing has been spent: release
			// the throttle and let the next prune tick try again rather
			// than parking the mention for a full push interval.
			h.mu.Lock()
			delete(h.lastPush, push.idHex)
			h.mu.Unlock()
			h.log.Printf("mentions: no route to %s… yet (%d pending, will retry next tick): %v",
				push.idHex[:8], push.count, err)
			continue
		}
		if err != nil {
			// Expected often enough not to be noise at the default level:
			// an unreachable peer, or one whose LXMF identity differs
			// from their RRC one.
			h.log.Printf("mentions: could not reach %s… over LXMF (%d pending, staying queued): %v",
				push.idHex[:8], push.count, err)
			continue
		}
		h.mu.Lock()
		if p, ok := h.peers[push.idHex]; ok {
			// Drop exactly what was sent, by sequence rather than by
			// count. The queue evicts from the front when it is full,
			// so if it was at its limit and more mentions arrived
			// during the upload, the first push.count entries are no
			// longer the ones that went out — trimming by index would
			// throw away the new arrivals instead.
			kept := p.Mentions[:0]
			for _, m := range p.Mentions {
				if m.Seq > push.highSeq {
					kept = append(kept, m)
				}
			}
			p.Mentions = kept
			if len(p.Mentions) == 0 {
				p.Mentions = nil
			}
			h.peersDirty = true
		}
		h.mu.Unlock()
		h.log.Printf("mentions: delivered %d notification(s) to %s… over LXMF",
			push.count, push.idHex[:8])
	}
}

// sweepLastPushLocked forgets throttle entries that can no longer
// suppress anything. Without it the map is the one piece of per-peer
// state with no bound: MaxKnownPeers caps the directory and eviction
// drops peers from it, but their throttle entries would stay forever.
// Caller must hold h.mu.
func (h *Hub) sweepLastPushLocked(now time.Time) {
	for id, last := range h.lastPush {
		if now.Sub(last) >= mentionPushInterval {
			delete(h.lastPush, id)
		}
	}
}

// renderMentionNotification turns pending mentions into the message an
// LXMF client will show.
//
// It has to stand on its own: it lands in a general-purpose messaging
// app next to unrelated conversations, so it says which hub it came
// from and quotes enough to be worth acting on.
func renderMentionNotification(hubName string, mentions []peerreg.Mention) (title, body string) {
	if hubName == "" {
		hubName = "RRC hub"
	}
	if len(mentions) == 1 {
		title = fmt.Sprintf("%s: mentioned in %s", hubName, mentions[0].Room)
	} else {
		title = fmt.Sprintf("%s: %d mentions", hubName, len(mentions))
	}

	var b strings.Builder
	for i, m := range mentions {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s in %s by %s:\n%s\n",
			humanAgo(time.Since(time.Unix(int64(m.TS), 0))),
			m.Room, mentionAuthor(m.ByNick, m.ByHex), m.Text)
	}
	return title, b.String()
}
