package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// resourceExpectation is a pending inbound RNS Resource transfer the
// client announced via a RESOURCE_ENVELOPE.
type resourceExpectation struct {
	id        []byte
	kind      string
	size      int
	sha256    []byte
	room      string
	srcSender []byte // the announcing peer's identity
	expiresMs int64
}

// handleResourceEnvelope processes a T_RESOURCE_ENVELOPE (type 50). It is
// accepted even pre-WELCOME.
func (s *Session) handleResourceEnvelope(env *rrc.Envelope) {
	h := s.hub
	if !h.cfg.EnableResourceTransfer {
		s.sendError(nil, "resource transfer disabled")
		return
	}
	if _, ok := env.Body.(map[any]any); !ok {
		s.sendError(nil, "invalid resource envelope body")
		return
	}
	info, ok := rrc.ResourceEnvelopeInfo(env)
	if !ok {
		// Distinguish missing id/kind/size for a clearer error.
		body, _ := env.Body.(map[any]any)
		if !hasKey(body, rrc.BResID) {
			s.sendError(nil, "resource envelope missing id")
			return
		}
		if !hasKey(body, rrc.BResKind) {
			s.sendError(nil, "resource envelope missing kind")
			return
		}
		s.sendError(nil, "resource envelope invalid size")
		return
	}
	if h.cfg.MaxResourceBytes > 0 && info.Size > h.cfg.MaxResourceBytes {
		h.statInc(&h.stats.resourcesRejected)
		s.sendError(nil, fmt.Sprintf("resource too large: %d > %d", info.Size, h.cfg.MaxResourceBytes))
		return
	}

	ttl := h.cfg.ResourceExpectationTTL.Duration
	if ttl <= 0 {
		ttl = resourceExpectFloor
	}
	exp := &resourceExpectation{
		id:        info.ID,
		kind:      info.Kind,
		size:      info.Size,
		sha256:    info.SHA256,
		room:      rrc.RoomName(env),
		srcSender: s.identity(),
		expiresMs: h.now() + ttl.Milliseconds(),
	}

	s.mu.Lock()
	if h.cfg.MaxPendingResourceExpectations > 0 &&
		len(s.expectations) >= h.cfg.MaxPendingResourceExpectations {
		s.mu.Unlock()
		s.sendError(nil, "too many pending resource expectations")
		return
	}
	s.expectations = append(s.expectations, exp)
	s.mu.Unlock()
	h.statInc(&h.stats.resourcesRecv)
}

// hasKey reports whether a decoded CBOR map has an unsigned integer key.
func hasKey(m map[any]any, key int) bool {
	for k := range m {
		switch v := k.(type) {
		case uint64:
			if v == uint64(key) {
				return true
			}
		case int64:
			if v == int64(key) {
				return true
			}
		case int:
			if v == key {
				return true
			}
		}
	}
	return false
}

// OnResourceConcluded is called when an RNS Resource the client
// advertised has fully arrived. payload is the reassembled bytes.
func (s *Session) OnResourceConcluded(payload []byte) {
	h := s.hub
	now := h.now()

	s.mu.Lock()
	var matched *resourceExpectation
	idx := -1
	for i, e := range s.expectations {
		if e.size != len(payload) {
			continue
		}
		if len(e.sha256) > 0 {
			sum := sha256.Sum256(payload)
			if hex.EncodeToString(sum[:]) != hex.EncodeToString(e.sha256) {
				continue
			}
		}
		matched = e
		idx = i
		break
	}
	if matched != nil {
		s.expectations = append(s.expectations[:idx], s.expectations[idx+1:]...)
	}
	s.mu.Unlock()
	_ = now

	if matched == nil {
		h.log.Printf("resource: concluded payload matched no pending expectation (%d bytes)", len(payload))
		return
	}

	switch matched.kind {
	case rrc.ResKindNotice:
		room := matched.room
		var recipients []Link
		h.mu.Lock()
		if r := h.roomLocked(room); r != nil {
			recipients = roomLinksExceptLocked(r, s)
		}
		h.mu.Unlock()
		notice := rrc.Notice(matched.srcSender, h.now(), &room, string(payload))
		h.fanout(recipients, notice)
		h.statInc(&h.stats.noticesFwd)
	case rrc.ResKindMOTD:
		h.log.Printf("resource: received motd (%d bytes)", len(payload))
	case rrc.ResKindBlob:
		h.log.Printf("resource: received blob (%d bytes)", len(payload))
	default:
		h.log.Printf("resource: received unknown kind %q (%d bytes)", matched.kind, len(payload))
	}
}

// reapExpectations drops expired pending expectations for this session.
func (s *Session) reapExpectations(nowMs int64) {
	s.mu.Lock()
	kept := s.expectations[:0]
	for _, e := range s.expectations {
		if e.expiresMs > nowMs {
			kept = append(kept, e)
		}
	}
	s.expectations = kept
	s.mu.Unlock()
}

// tryResourceSend attempts to deliver payload as an RNS Resource. It first
// sends a RESOURCE_ENVELOPE, then calls Link.SendResource. Returns false
// (caller falls back to chunked NOTICEs) when resource transfer is
// unavailable.
func (s *Session) tryResourceSend(payload []byte, kind string, room *string) bool {
	h := s.hub
	if !h.cfg.EnableResourceTransfer {
		return false
	}
	sum := sha256.Sum256(payload)
	rid := rrc.FreshID()
	env := rrc.ResourceEnvelope(h.identityHash, h.now(), room, rid, kind,
		len(payload), sum[:], "")
	s.send(env)
	if err := s.link.SendResource(payload); err != nil {
		h.log.Printf("resource: SendResource failed, falling back to chunks: %v", err)
		return false
	}
	h.statInc(&h.stats.resourcesSent)
	return true
}
