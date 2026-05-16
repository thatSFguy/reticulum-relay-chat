package rrc

import (
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// Envelope is the CBOR map every RRC message is wrapped in (see
// rrcd/envelope.py make_envelope / validate_envelope).
//
// Wire form: a CBOR map with unsigned-integer keys KV..KNick, emitted in
// ascending order — which is CBOR's canonical map-key order, so Encode
// output is byte-identical to the Python hub and the Kotlin client.
//
// Src is the sender's RNS identity hash (16 bytes) and is opaque — copy
// the bytes through verbatim, never decode-and-re-encode (the same
// identity-hash-vs-destination-hash trap LXMF has).
//
// Room, Body and Nick are optional: a nil pointer / nil interface means
// the key is absent on the wire, exactly as make_envelope omits a key
// whose value is null.
type Envelope struct {
	Version     int
	Type        int
	MsgID       []byte
	TimestampMs int64
	Src         []byte
	Room        *string
	Body        any
	Nick        *string
}

var canonicalEnc cbor.EncMode

func init() {
	em, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic("rrc: cbor canonical enc mode: " + err.Error())
	}
	canonicalEnc = em
}

// FreshID returns MsgIDLength fresh random bytes for an envelope KID.
func FreshID() []byte {
	b := make([]byte, MsgIDLength)
	if _, err := rand.Read(b); err != nil {
		panic("rrc: crypto/rand failed: " + err.Error())
	}
	return b
}

// strptr is a small helper for the optional string fields.
func strptr(s string) *string { return &s }

// Encode produces canonical CBOR wire bytes. Optional fields (Room,
// Body, Nick) are omitted entirely when nil.
func (e *Envelope) Encode() ([]byte, error) {
	m := map[int]any{
		KV:   e.Version,
		KT:   e.Type,
		KID:  e.MsgID,
		KTS:  e.TimestampMs,
		KSrc: e.Src,
	}
	if e.Room != nil {
		m[KRoom] = *e.Room
	}
	if e.Body != nil {
		m[KBody] = e.Body
	}
	if e.Nick != nil {
		m[KNick] = *e.Nick
	}
	return canonicalEnc.Marshal(m)
}

// Decode parses + validates RRC wire bytes into an Envelope. It mirrors
// validate_envelope: non-integer / negative keys, a missing required
// key, an unsupported version, or a wrong-typed required value are all
// rejected.
func Decode(b []byte) (*Envelope, error) {
	var raw map[any]any
	if err := cbor.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("rrc: envelope is not a CBOR map: %w", err)
	}
	return fromMap(raw)
}

func fromMap(m map[any]any) (*Envelope, error) {
	for k := range m {
		if _, ok := asUint(k); !ok {
			return nil, errors.New("rrc: envelope keys must be unsigned integers")
		}
	}

	version, err := reqInt(m, KV, "protocol version")
	if err != nil {
		return nil, err
	}
	if version != Version {
		return nil, fmt.Errorf("rrc: unsupported protocol version %d", version)
	}
	typ, err := reqInt(m, KT, "message type")
	if err != nil {
		return nil, err
	}
	msgID, err := reqBytes(m, KID, "message id")
	if err != nil {
		return nil, err
	}
	ts, err := reqInt64(m, KTS, "timestamp")
	if err != nil {
		return nil, err
	}
	if ts < 0 {
		return nil, errors.New("rrc: timestamp must be unsigned")
	}
	src, err := reqBytes(m, KSrc, "sender identity")
	if err != nil {
		return nil, err
	}
	room, err := optString(m, KRoom, "room name")
	if err != nil {
		return nil, err
	}
	nick, err := optString(m, KNick, "nickname")
	if err != nil {
		return nil, err
	}

	return &Envelope{
		Version:     version,
		Type:        typ,
		MsgID:       msgID,
		TimestampMs: ts,
		Src:         src,
		Room:        room,
		Body:        valueOf(m, KBody),
		Nick:        nick,
	}, nil
}

// --- map helpers — CBOR decode yields uint64 keys, builders use int ---

func asUint(k any) (uint64, bool) {
	switch v := k.(type) {
	case uint64:
		return v, true
	case int64:
		if v >= 0 {
			return uint64(v), true
		}
	case int:
		if v >= 0 {
			return uint64(v), true
		}
	}
	return 0, false
}

func valueOf(m map[any]any, key int) any {
	for k, v := range m {
		if u, ok := asUint(k); ok && u == uint64(key) {
			return v
		}
	}
	return nil
}

func reqInt(m map[any]any, key int, what string) (int, error) {
	v, err := reqInt64(m, key, what)
	return int(v), err
}

func reqInt64(m map[any]any, key int, what string) (int64, error) {
	switch v := valueOf(m, key).(type) {
	case uint64:
		return int64(v), nil
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	default:
		return 0, fmt.Errorf("rrc: missing or non-integer %s", what)
	}
}

func reqBytes(m map[any]any, key int, what string) ([]byte, error) {
	if b, ok := valueOf(m, key).([]byte); ok {
		return b, nil
	}
	return nil, fmt.Errorf("rrc: missing or non-bytes %s", what)
}

func optString(m map[any]any, key int, what string) (*string, error) {
	v := valueOf(m, key)
	if v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("rrc: %s must be a string", what)
	}
	return &s, nil
}
