package rrc

// Hub-side message builders (hub → client) and body extractors for the
// inbound client messages (client → hub) the hub must read. Mirrors
// rrcd/messages.py.

// newEnvelope stamps the common envelope fields with a fresh KID.
func newEnvelope(typ int, src []byte, tsMs int64) *Envelope {
	return &Envelope{
		Version:     Version,
		Type:        typ,
		MsgID:       FreshID(),
		TimestampMs: tsMs,
		Src:         src,
	}
}

// --- builders: hub → client ------------------------------------------

// Welcome builds the hub's reply to a client HELLO.
func Welcome(hubSrc []byte, tsMs int64, hubName, hubVersion string, lim Limits, resourceCapable bool) *Envelope {
	caps := map[int]any{}
	if resourceCapable {
		caps[CapResourceEnvelope] = 1
	}
	e := newEnvelope(TWelcome, hubSrc, tsMs)
	e.Body = map[int]any{
		BWelcomeHub:    hubName,
		BWelcomeVer:    hubVersion,
		BWelcomeCaps:   caps,
		BWelcomeLimits: limitsToMap(lim),
	}
	return e
}

func limitsToMap(l Limits) map[int]any {
	return map[int]any{
		BLimitMaxNickBytes:        l.MaxNickBytes,
		BLimitMaxRoomNameBytes:    l.MaxRoomNameBytes,
		BLimitMaxMsgBodyBytes:     l.MaxMsgBodyBytes,
		BLimitMaxRoomsPerSession:  l.MaxRoomsPerSession,
		BLimitRateLimitMsgsPerMin: l.RateLimitMsgsPerMin,
	}
}

// Joined announces the post-change member set of a room. members is a
// list of 16-byte RNS identity hashes.
func Joined(hubSrc []byte, tsMs int64, room string, members [][]byte) *Envelope {
	e := newEnvelope(TJoined, hubSrc, tsMs)
	e.Room = strptr(room)
	e.Body = membersToList(members)
	return e
}

// Parted announces the post-change member set after a PART.
func Parted(hubSrc []byte, tsMs int64, room string, members [][]byte) *Envelope {
	e := newEnvelope(TParted, hubSrc, tsMs)
	e.Room = strptr(room)
	e.Body = membersToList(members)
	return e
}

func membersToList(members [][]byte) []any {
	out := make([]any, len(members))
	for i, m := range members {
		out[i] = m
	}
	return out
}

// Notice is an informational, non-error message — room may be nil for a
// hub-wide notice (e.g. the chunked greeting after WELCOME).
func Notice(hubSrc []byte, tsMs int64, room *string, text string) *Envelope {
	e := newEnvelope(TNotice, hubSrc, tsMs)
	e.Room = room
	e.Body = text
	return e
}

// Error reports a failed client request. room may be nil.
func Error(hubSrc []byte, tsMs int64, room *string, text string) *Envelope {
	e := newEnvelope(TError, hubSrc, tsMs)
	e.Room = room
	e.Body = text
	return e
}

// Ping is a hub-initiated keepalive; the client echoes payload in a PONG.
func Ping(hubSrc []byte, tsMs int64, payload []byte) *Envelope {
	e := newEnvelope(TPing, hubSrc, tsMs)
	if payload != nil {
		e.Body = payload
	}
	return e
}

// Pong answers a client-initiated PING, echoing its payload.
func Pong(hubSrc []byte, tsMs int64, payload []byte) *Envelope {
	e := newEnvelope(TPong, hubSrc, tsMs)
	if payload != nil {
		e.Body = payload
	}
	return e
}

// --- body extractors: client → hub -----------------------------------

// HelloInfo extracts the advertised client name/version and resource
// capability from a decoded HELLO. Missing fields yield zero values — a
// misbehaving client must never cause an error here.
func HelloInfo(e *Envelope) (name, version string, resourceCapable bool) {
	body, _ := e.Body.(map[any]any)
	if body == nil {
		return
	}
	if s, ok := valueOf(body, BHelloName).(string); ok {
		name = s
	}
	if s, ok := valueOf(body, BHelloVer).(string); ok {
		version = s
	}
	if caps, ok := valueOf(body, BHelloCaps).(map[any]any); ok {
		resourceCapable = valueOf(caps, CapResourceEnvelope) != nil
	}
	return
}

// BodyText returns the envelope body as a string (MSG / NOTICE / ERROR),
// or "" when absent or not a string.
func BodyText(e *Envelope) string {
	s, _ := e.Body.(string)
	return s
}

// BodyBytes returns the envelope body as a byte slice (PING / PONG
// payload), or nil when absent or not bytes.
func BodyBytes(e *Envelope) []byte {
	b, _ := e.Body.([]byte)
	return b
}

// RoomName returns the envelope room name, or "" when the key is absent.
func RoomName(e *Envelope) string {
	if e.Room == nil {
		return ""
	}
	return *e.Room
}

// NickName returns the envelope nick, or "" when the key is absent.
func NickName(e *Envelope) string {
	if e.Nick == nil {
		return ""
	}
	return *e.Nick
}
