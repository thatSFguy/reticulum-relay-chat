package hub

import (
	"fmt"
	"sort"
	"sync/atomic"
)

// stats holds the hub's cumulative event counters. All fields are
// accessed atomically so background loops and inbound handlers may
// update them without the hub lock.
type stats struct {
	joins       int64
	parts       int64
	msgsFwd     int64
	noticesFwd  int64
	actionsFwd  int64
	errorsSent  int64
	rateLimited int64

	pingsIn  int64
	pingsOut int64
	pongsIn  int64
	pongsOut int64

	resourcesSent     int64
	resourcesRecv     int64
	resourcesRejected int64

	bytesIn  int64
	bytesOut int64
	pktsIn   int64
	pktsBad  int64
}

func (h *Hub) statInc(p *int64)          { atomic.AddInt64(p, 1) }
func (h *Hub) statAdd(p *int64, n int64) { atomic.AddInt64(p, n) }

// snapshotStats renders the /stats NOTICE body. Lines are newline-joined
// (rrcd has a bug joining with ""; this fixes it).
func (h *Hub) snapshotStats() string {
	h.mu.Lock()
	total := len(h.sessions)
	identified := 0
	welcomed := 0
	memberships := 0
	roomCount := len(h.rooms)
	type rc struct {
		name string
		n    int
	}
	var topRooms []rc
	for s := range h.sessions {
		if s.identity() != nil {
			identified++
		}
		s.mu.Lock()
		if s.welcomed {
			welcomed++
		}
		memberships += len(s.joined)
		s.mu.Unlock()
	}
	for name, r := range h.rooms {
		topRooms = append(topRooms, rc{name, len(r.members)})
	}
	trustedN := len(h.trusted)
	bannedN := len(h.banned)
	klineN := len(h.klines)
	h.mu.Unlock()

	sort.Slice(topRooms, func(i, j int) bool {
		if topRooms[i].n != topRooms[j].n {
			return topRooms[i].n > topRooms[j].n
		}
		return topRooms[i].name < topRooms[j].name
	})
	top := ""
	for i, r := range topRooms {
		if i >= 5 {
			break
		}
		if top != "" {
			top += ", "
		}
		top += fmt.Sprintf("%s(%d)", r.name, r.n)
	}
	if top == "" {
		top = "(none)"
	}

	uptime := (h.now() - h.startedAt) / 1000
	st := &h.stats
	lines := []string{
		fmt.Sprintf("hub stats: version=%s", h.cfg.Version),
		fmt.Sprintf("uptime_s=%d", uptime),
		fmt.Sprintf("clients: total=%d identified=%d welcomed=%d", total, identified, welcomed),
		fmt.Sprintf("rooms=%d memberships=%d", roomCount, memberships),
		fmt.Sprintf("top_rooms: %s", top),
		fmt.Sprintf("trust: trusted=%d banned=%d klines=%d", trustedN, bannedN, klineN),
		fmt.Sprintf("limits: nick=%d room=%d body=%d rooms_per_session=%d rate=%d",
			h.limits.MaxNickBytes, h.limits.MaxRoomNameBytes, h.limits.MaxMsgBodyBytes,
			h.limits.MaxRoomsPerSession, h.limits.RateLimitMsgsPerMin),
		fmt.Sprintf("features: resource_transfer=%v include_member_list=%v",
			h.cfg.EnableResourceTransfer, h.cfg.IncludeJoinedMemberList),
		fmt.Sprintf("io: bytes_in=%d bytes_out=%d pkts_in=%d pkts_bad=%d",
			atomic.LoadInt64(&st.bytesIn), atomic.LoadInt64(&st.bytesOut),
			atomic.LoadInt64(&st.pktsIn), atomic.LoadInt64(&st.pktsBad)),
		fmt.Sprintf("events: joins=%d parts=%d msgs_fwd=%d notices_fwd=%d actions_fwd=%d errors_sent=%d rate_limited=%d",
			atomic.LoadInt64(&st.joins), atomic.LoadInt64(&st.parts),
			atomic.LoadInt64(&st.msgsFwd), atomic.LoadInt64(&st.noticesFwd),
			atomic.LoadInt64(&st.actionsFwd), atomic.LoadInt64(&st.errorsSent),
			atomic.LoadInt64(&st.rateLimited)),
		fmt.Sprintf("ping: in=%d out=%d pong_in=%d pong_out=%d",
			atomic.LoadInt64(&st.pingsIn), atomic.LoadInt64(&st.pingsOut),
			atomic.LoadInt64(&st.pongsIn), atomic.LoadInt64(&st.pongsOut)),
		fmt.Sprintf("resources: sent=%d received=%d rejected=%d",
			atomic.LoadInt64(&st.resourcesSent), atomic.LoadInt64(&st.resourcesRecv),
			atomic.LoadInt64(&st.resourcesRejected)),
	}
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}
