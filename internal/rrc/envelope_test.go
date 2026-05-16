package rrc

import (
	"bytes"
	"testing"
)

// TestEnvelopeRoundTrip encodes a fully-populated envelope and decodes
// it back, asserting every field survives the CBOR round trip.
func TestEnvelopeRoundTrip(t *testing.T) {
	src := bytes.Repeat([]byte{0xAB}, 16)
	room := "lobby"
	nick := "alice"
	orig := &Envelope{
		Version:     Version,
		Type:        TMsg,
		MsgID:       []byte{1, 2, 3, 4, 5, 6, 7, 8},
		TimestampMs: 1_700_000_000_000,
		Src:         src,
		Room:        &room,
		Body:        "hello world",
		Nick:        &nick,
	}
	wire, err := orig.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(wire)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Version != Version || got.Type != TMsg {
		t.Errorf("version/type drift: %d/%d", got.Version, got.Type)
	}
	if !bytes.Equal(got.MsgID, orig.MsgID) {
		t.Errorf("msgId drift: %x", got.MsgID)
	}
	if got.TimestampMs != orig.TimestampMs {
		t.Errorf("timestamp drift: %d", got.TimestampMs)
	}
	if !bytes.Equal(got.Src, src) {
		t.Errorf("src drift: %x", got.Src)
	}
	if got.Room == nil || *got.Room != room {
		t.Errorf("room drift: %v", got.Room)
	}
	if got.Nick == nil || *got.Nick != nick {
		t.Errorf("nick drift: %v", got.Nick)
	}
	if s, _ := got.Body.(string); s != "hello world" {
		t.Errorf("body drift: %v", got.Body)
	}
}

// TestOptionalKeysOmitted proves a minimal envelope omits the optional
// Room/Body/Nick keys entirely — the wire map carries exactly the five
// required keys, encoded as a canonical CBOR map (0xA5 = map of 5).
func TestOptionalKeysOmitted(t *testing.T) {
	e := &Envelope{
		Version:     Version,
		Type:        THello,
		MsgID:       FreshID(),
		TimestampMs: 1,
		Src:         bytes.Repeat([]byte{0x01}, 16),
	}
	wire, err := e.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if wire[0] != 0xA5 {
		t.Fatalf("expected a 5-pair CBOR map header 0xA5, got 0x%02X", wire[0])
	}
	// Canonical order: the five required keys 0..4 follow as single
	// CBOR unsigned-int bytes 0x00..0x04 at the head of each pair.
	got, err := Decode(wire)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Room != nil || got.Body != nil || got.Nick != nil {
		t.Errorf("optional keys should decode as absent: room=%v body=%v nick=%v",
			got.Room, got.Body, got.Nick)
	}
}

// TestDecodeRejectsBadVersion ensures an envelope carrying an
// unsupported protocol version is rejected (mirrors validate_envelope).
func TestDecodeRejectsBadVersion(t *testing.T) {
	e := &Envelope{
		Version:     99,
		Type:        THello,
		MsgID:       FreshID(),
		TimestampMs: 1,
		Src:         bytes.Repeat([]byte{0x01}, 16),
	}
	wire, err := e.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := Decode(wire); err == nil {
		t.Fatal("Decode accepted an unsupported protocol version")
	}
}

// TestDecodeRejectsMissingRequiredKey ensures a required key cannot be
// dropped — an envelope with no KSrc must fail to decode.
func TestDecodeRejectsMissingRequiredKey(t *testing.T) {
	// Encode a map missing KSrc directly via the canonical encoder.
	m := map[int]any{KV: Version, KT: THello, KID: FreshID(), KTS: 1}
	wire, err := canonicalEnc.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := Decode(wire); err == nil {
		t.Fatal("Decode accepted an envelope missing the required KSrc key")
	}
}

// TestWelcomeBuilder checks the hub WELCOME builder produces a decodable
// envelope whose body carries the advertised hub name and limits.
func TestWelcomeBuilder(t *testing.T) {
	hubSrc := bytes.Repeat([]byte{0x07}, 16)
	lim := DefaultLimits()
	w := Welcome(hubSrc, 1234, "Test Hub", "rrc-hub-go/0.1.0", lim, true)
	wire, err := w.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(wire)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Type != TWelcome {
		t.Fatalf("type: got %d want %d", got.Type, TWelcome)
	}
	body, ok := got.Body.(map[any]any)
	if !ok {
		t.Fatalf("WELCOME body is not a map: %T", got.Body)
	}
	if name, _ := valueOf(body, BWelcomeHub).(string); name != "Test Hub" {
		t.Errorf("hub name: got %q", name)
	}
	limits, ok := valueOf(body, BWelcomeLimits).(map[any]any)
	if !ok {
		t.Fatalf("WELCOME limits is not a map: %T", valueOf(body, BWelcomeLimits))
	}
	if mb, _ := asUint(valueOf(limits, BLimitMaxMsgBodyBytes)); mb != uint64(lim.MaxMsgBodyBytes) {
		t.Errorf("max_msg_body_bytes: got %d want %d", mb, lim.MaxMsgBodyBytes)
	}
}
