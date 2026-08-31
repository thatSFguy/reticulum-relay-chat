package rrc

// The `rrc.hub` announce app_data (SPEC §4.6).
//
// This is the only part of RRC a peer reads WITHOUT a session: it is
// how a client lists hubs it has never connected to, so its encoding is
// an interop surface rather than an internal detail.
//
// The Go hub used to announce the hub name as bare UTF-8 while `rrcd`,
// the Python reference hub, announced the CBOR map below — and a
// receiver has no way to tell the two apart, because a CBOR decoder
// reading a bare name consumes its first LETTER as an item header.
// "Michmesh RRC Hub" begins `0x4d`, a byte string of length 13, so a
// decoder that stops at the first item returns "ichmesh RRC H" and
// raises nothing. Measured on a live mesh 2026-08-31: of 54 announcing
// hubs, 51 sent the CBOR map and the 3 bare ones were all Go hubs.
//
// So the map is what a client can actually rely on, and RRC is CBOR
// throughout in any case (see envelope.go) — msgpack, the LXMF §4.3
// app_data codec, is NOT used here.
const (
	hubAppDataProto   = "rrc"
	hubAppDataVersion = 1
)

// HubAppData encodes the app_data for an rrc.hub announce: the CBOR map
// {"proto": "rrc", "v": 1, "hub": name}.
//
// Canonical encoding orders the keys "v", "hub", "proto" by length.
// That is a different order from the one rrcd happens to emit, and it
// does not matter — a CBOR map is unordered and rrcd's own announces
// are observed in both orders on the wire.
func HubAppData(name string) ([]byte, error) {
	return canonicalEnc.Marshal(map[string]any{
		"proto": hubAppDataProto,
		"v":     hubAppDataVersion,
		"hub":   name,
	})
}
