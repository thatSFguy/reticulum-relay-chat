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

// Joined announces a join. members is the post-change member set (16-byte
// RNS identity hashes); pass nil to omit the member list entirely — the
// hub does so when include_joined_member_list is off.
func Joined(hubSrc []byte, tsMs int64, room string, members [][]byte) *Envelope {
	e := newEnvelope(TJoined, hubSrc, tsMs)
	e.Room = strptr(room)
	e.Body = membersToList(members)
	return e
}

// Parted announces a departure. members behaves as in Joined.
func Parted(hubSrc []byte, tsMs int64, room string, members [][]byte) *Envelope {
	e := newEnvelope(TParted, hubSrc, tsMs)
	e.Room = strptr(room)
	e.Body = membersToList(members)
	return e
}

// membersToList renders the member list, or nil (key omitted) when
// members is nil.
func membersToList(members [][]byte) any {
	if members == nil {
		return nil
	}
	out := make([]any, len(members))
	for i, m := range members {
		out[i] = m
	}
	return out
}

// ResourceEnvelope announces an RNS Resource transfer carrying a payload
// too large for a single packet (rrcd resources.py). rid is 8 random
// bytes; kind is one of the ResKind* constants; sha256 may be nil;
// encoding may be "".
func ResourceEnvelope(src []byte, tsMs int64, room *string, rid []byte, kind string, size int, sha256 []byte, encoding string) *Envelope {
	e := newEnvelope(TResourceEnvelope, src, tsMs)
	e.Room = room
	body := map[int]any{
		BResID:   rid,
		BResKind: kind,
		BResSize: size,
	}
	if sha256 != nil {
		body[BResSHA256] = sha256
	}
	if encoding != "" {
		body[BResEncoding] = encoding
	}
	e.Body = body
	return e
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

// LegacyHelloNick returns the nickname a pre-0.1.1 client carried in the
// HELLO body (key BHelloNickLegacy), or "" when absent.
func LegacyHelloNick(e *Envelope) string {
	body, _ := e.Body.(map[any]any)
	if body == nil {
		return ""
	}
	s, _ := valueOf(body, BHelloNickLegacy).(string)
	return s
}

// ResourceInfo is the parsed body of a RESOURCE_ENVELOPE message.
type ResourceInfo struct {
	ID       []byte // resource id
	Kind     string // ResKind*
	Size     int    // declared payload size in bytes
	SHA256   []byte // payload digest, nil when the client omitted it
	Encoding string // encoding hint, "" when absent
}

// ResourceEnvelopeInfo extracts a RESOURCE_ENVELOPE body. ok is false
// when the body is absent, not a CBOR map, or omits/mistypes a required
// field (id, kind, size) — the caller answers such a frame with ERROR.
func ResourceEnvelopeInfo(e *Envelope) (ResourceInfo, bool) {
	body, _ := e.Body.(map[any]any)
	if body == nil {
		return ResourceInfo{}, false
	}
	var r ResourceInfo
	id, ok := valueOf(body, BResID).([]byte)
	if !ok {
		return ResourceInfo{}, false
	}
	r.ID = id
	kind, ok := valueOf(body, BResKind).(string)
	if !ok {
		return ResourceInfo{}, false
	}
	r.Kind = kind
	switch s := valueOf(body, BResSize).(type) {
	case uint64:
		r.Size = int(s)
	case int64:
		r.Size = int(s)
	case int:
		r.Size = s
	default:
		return ResourceInfo{}, false
	}
	if r.Size < 0 {
		return ResourceInfo{}, false
	}
	if sha, ok := valueOf(body, BResSHA256).([]byte); ok {
		r.SHA256 = sha
	}
	if enc, ok := valueOf(body, BResEncoding).(string); ok {
		r.Encoding = enc
	}
	return r, true
}
