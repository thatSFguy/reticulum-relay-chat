// Package rrc implements the Reticulum Relay Chat wire protocol — the
// CBOR envelope, message types, and limits exchanged between an RRC
// client and an RRC hub over an established Reticulum Link.
//
// Authoritative spec: https://rrc.kc1awv.net/. This package is a Go
// port of the verified Kotlin implementation in the reticulum-mobile-app
// repo (shared/.../rrc) and mirrors the reference hub rrcd
// (github.com/kc1awv/rrcd). Numeric keys and type codes are part of the
// wire format — do not renumber.
package rrc

// Version is the RRC protocol version carried in envelope key KV.
const Version = 1

// Envelope keys — a CBOR map with unsigned-integer keys.
const (
	KV    = 0 // protocol version (int)
	KT    = 1 // message type (int)
	KID   = 2 // message id (8 random bytes)
	KTS   = 3 // timestamp, ms since epoch (uint)
	KSrc  = 4 // sender identity hash (16 bytes) — opaque, never re-encode
	KRoom = 5 // room name (string, optional)
	KBody = 6 // body (type-specific, optional)
	KNick = 7 // nickname (string, optional)
)

// Message types.
const (
	THello            = 1
	TWelcome          = 2
	TJoin             = 10
	TJoined           = 11
	TPart             = 12
	TParted           = 13
	TMsg              = 20
	TNotice           = 21
	TPing             = 30
	TPong             = 31
	TError            = 40
	TResourceEnvelope = 50
)

// HELLO body keys.
const (
	BHelloName = 0
	BHelloVer  = 1
	BHelloCaps = 2
)

// WELCOME body keys.
const (
	BWelcomeHub    = 0
	BWelcomeVer    = 1
	BWelcomeCaps   = 2
	BWelcomeLimits = 3
)

// Hub limits map keys (inside the WELCOME body's BWelcomeLimits map).
const (
	BLimitMaxNickBytes        = 0
	BLimitMaxRoomNameBytes    = 1
	BLimitMaxMsgBodyBytes     = 2
	BLimitMaxRoomsPerSession  = 3
	BLimitRateLimitMsgsPerMin = 4
)

// Capability map keys (values are advisory — presence is what counts).
const CapResourceEnvelope = 0

// MsgIDLength is the envelope KID length — os.urandom(8) in rrcd.
const MsgIDLength = 8

// Limits is the hub-advertised limit set carried in WELCOME. Zero values
// are replaced with DefaultLimits when advertised.
type Limits struct {
	MaxNickBytes        int
	MaxRoomNameBytes    int
	MaxMsgBodyBytes     int
	MaxRoomsPerSession  int
	RateLimitMsgsPerMin int
}

// DefaultLimits are the conservative defaults a hub advertises unless
// overridden by configuration.
func DefaultLimits() Limits {
	return Limits{
		MaxNickBytes:        32,
		MaxRoomNameBytes:    64,
		MaxMsgBodyBytes:     4096,
		MaxRoomsPerSession:  16,
		RateLimitMsgsPerMin: 30,
	}
}
