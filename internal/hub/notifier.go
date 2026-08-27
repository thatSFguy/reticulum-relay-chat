package hub

import (
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

// SetOfflineNotifier installs the notifier. Call before Start.
func (h *Hub) SetOfflineNotifier(n OfflineNotifier) {
	h.mu.Lock()
	h.notifier = n
	h.mu.Unlock()
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
	title  string
	body   string
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
			idHex:  idHex,
			pubKey: append([]byte(nil), p.PublicKey...),
			count:  len(p.Mentions),
			title:  title,
			body:   body,
		})
	}
	h.mu.Unlock()

	for _, push := range batch {
		if err := n.NotifyAbsent(push.pubKey, push.title, push.body); err != nil {
			// Expected often enough not to be noise at the default level:
			// no node reachable, or a peer whose LXMF identity differs
			// from their RRC one.
			h.log.Printf("mentions: could not reach %s… over LXMF (%d pending, staying queued): %v",
				push.idHex[:8], push.count, err)
			continue
		}
		h.mu.Lock()
		if p, ok := h.peers[push.idHex]; ok {
			// Drop exactly what was sent. Anything appended while the
			// upload was in flight is left for the next round rather
			// than silently discarded.
			if push.count >= len(p.Mentions) {
				p.Mentions = nil
			} else {
				p.Mentions = p.Mentions[push.count:]
			}
			h.peersDirty = true
		}
		h.mu.Unlock()
		h.log.Printf("mentions: delivered %d notification(s) to %s… over LXMF",
			push.count, push.idHex[:8])
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
