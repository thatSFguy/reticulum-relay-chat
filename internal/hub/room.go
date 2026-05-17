package hub

import (
	"encoding/hex"
	"sort"

	"github.com/thatSFguy/reticulum-relay-chat/internal/roomreg"
)

// Room is one chat room. All fields are guarded by the owning Hub's
// mutex.
type Room struct {
	Name    string
	founder string // hex identity hash of the room founder
	topic   string

	// modes
	moderated     bool   // +m
	inviteOnly    bool   // +i
	topicOpsOnly  bool   // +t
	noOutsideMsgs bool   // +n
	private       bool   // +p
	key           string // +k key; "" = open
	registered    bool   // +r

	ops     map[string]struct{} // hex identity hashes
	voiced  map[string]struct{} // hex identity hashes
	bans    map[string]struct{} // hex identity hashes
	invited map[string]float64  // hex hash -> expiry unix seconds

	members    map[*Session]struct{}
	lastUsedTS float64 // unix seconds
}

// newRoom builds an empty room.
func newRoom(name string) *Room {
	return &Room{
		Name:    name,
		ops:     make(map[string]struct{}),
		voiced:  make(map[string]struct{}),
		bans:    make(map[string]struct{}),
		invited: make(map[string]float64),
		members: make(map[*Session]struct{}),
	}
}

// roomFromRecord materializes a persisted RoomRecord into a live Room.
func roomFromRecord(name string, rec *roomreg.RoomRecord) *Room {
	r := newRoom(name)
	r.founder = rec.Founder
	r.topic = rec.Topic
	r.moderated = rec.Moderated
	r.inviteOnly = rec.InviteOnly
	r.topicOpsOnly = rec.TopicOpsOnly
	r.noOutsideMsgs = rec.NoOutsideMsgs
	r.private = rec.Private
	r.key = rec.Key
	r.registered = true
	for _, o := range rec.Operators {
		r.ops[o] = struct{}{}
	}
	for _, v := range rec.Voiced {
		r.voiced[v] = struct{}{}
	}
	for _, b := range rec.Bans {
		r.bans[b] = struct{}{}
	}
	for h, exp := range rec.Invited {
		r.invited[h] = exp
	}
	r.lastUsedTS = rec.LastUsedTS
	return r
}

// toRecord renders a Room back to a persistable RoomRecord.
func (r *Room) toRecord() *roomreg.RoomRecord {
	return &roomreg.RoomRecord{
		Name:          r.Name,
		Founder:       r.founder,
		Topic:         r.topic,
		Moderated:     r.moderated,
		InviteOnly:    r.inviteOnly,
		TopicOpsOnly:  r.topicOpsOnly,
		NoOutsideMsgs: r.noOutsideMsgs,
		Private:       r.private,
		Key:           r.key,
		Operators:     sortedHexSet(r.ops),
		Voiced:        sortedHexSet(r.voiced),
		Bans:          sortedHexSet(r.bans),
		Invited:       copyInvited(r.invited),
		LastUsedTS:    r.lastUsedTS,
	}
}

// sortedHexSet returns the sorted keys of a hex-string set, or nil when
// empty.
func sortedHexSet(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func copyInvited(m map[string]float64) map[string]float64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// modeString renders the room's mode flags in the fixed order "i k m n p
// r t". Returns "(none)" when nothing is set.
func (r *Room) modeString() string {
	var b []byte
	if r.inviteOnly {
		b = append(b, 'i')
	}
	if r.key != "" {
		b = append(b, 'k')
	}
	if r.moderated {
		b = append(b, 'm')
	}
	if r.noOutsideMsgs {
		b = append(b, 'n')
	}
	if r.private {
		b = append(b, 'p')
	}
	if r.registered {
		b = append(b, 'r')
	}
	if r.topicOpsOnly {
		b = append(b, 't')
	}
	if len(b) == 0 {
		return "(none)"
	}
	return "+" + string(b)
}

// isOp reports whether the identity is a room operator: server-op, the
// founder, or in the ops set. h must hold the hub lock; serverOp is the
// already-computed server-op result.
func (r *Room) isOp(idHex string, serverOp bool) bool {
	// An empty identity is never an operator (A1): an un-identified peer
	// must not inherit ops, and a founderless room ("") confers nothing.
	if idHex == "" {
		return false
	}
	if serverOp {
		return true
	}
	if idHex == r.founder {
		return true
	}
	_, ok := r.ops[idHex]
	return ok
}

// isVoiced reports whether the identity may speak in a +m room.
func (r *Room) isVoiced(idHex string, serverOp bool) bool {
	if idHex == "" {
		return false
	}
	if r.isOp(idHex, serverOp) {
		return true
	}
	_, ok := r.voiced[idHex]
	return ok
}

// hasMember reports whether the session is currently joined to the room.
func (r *Room) hasMember(s *Session) bool {
	_, ok := r.members[s]
	return ok
}

// hasMemberHash reports whether a connected member with that identity
// hash is joined.
func (r *Room) hasMemberHash(idHex string) bool {
	for s := range r.members {
		if id := s.identity(); id != nil && hex.EncodeToString(id) == idHex {
			return true
		}
	}
	return false
}

// memberHashesLocked returns the room's sorted member identity hashes.
// Caller must hold the hub mutex.
func (r *Room) memberHashesLocked() [][]byte {
	out := make([][]byte, 0, len(r.members))
	for s := range r.members {
		if id := s.identity(); id != nil {
			out = append(out, append([]byte(nil), id...))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return hex.EncodeToString(out[i]) < hex.EncodeToString(out[j])
	})
	return out
}

// pruneInvitesLocked drops expired invite entries. Caller must hold the
// hub mutex.
func (r *Room) pruneInvitesLocked(nowUnix float64) {
	for h, exp := range r.invited {
		if exp <= nowUnix {
			delete(r.invited, h)
		}
	}
}

// hasValidInvite reports whether the identity holds a non-expired invite.
func (r *Room) hasValidInvite(idHex string, nowUnix float64) bool {
	exp, ok := r.invited[idHex]
	return ok && exp > nowUnix
}
