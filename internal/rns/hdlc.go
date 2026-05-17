package rns

import (
	"bufio"
	"errors"
	"io"
)

// HDLC byte-stuffing framing as used by TCPClientInterface (SPEC §8.2).
// FLAG delimits frames. ESC + (byte XOR MASK) escapes any in-band FLAG or
// ESC byte. Unlike KISS framing on serial lines, there is no leading
// command byte on the TCP wire — frames are raw Reticulum packets.
const (
	hdlcFlag    = 0x7E
	hdlcEsc     = 0x7D
	hdlcEscMask = 0x20
)

// maxHDLCFrameSize bounds the un-escaped payload of a single HDLC frame.
// A Reticulum packet on a TCP interface is normally near ReticulumMTU
// (500 bytes) and at most a few KiB even with link MTU discovery; 64 KiB
// is generous headroom for any legitimate frame while still capping a
// peer that streams an unterminated frame from OOMing the process before
// any authentication (audit A5). An over-cap frame is discarded and the
// decoder resyncs to the next flag.
const maxHDLCFrameSize = 64 * 1024

// EncodeHDLC wraps a packet in HDLC framing: FLAG || escape(p) || FLAG.
func EncodeHDLC(p []byte) []byte {
	out := make([]byte, 0, len(p)+2)
	out = append(out, hdlcFlag)
	for _, b := range p {
		if b == hdlcFlag || b == hdlcEsc {
			out = append(out, hdlcEsc, b^hdlcEscMask)
		} else {
			out = append(out, b)
		}
	}
	out = append(out, hdlcFlag)
	return out
}

// HDLCDecoder reads HDLC-framed frames from an underlying byte stream.
// Empty frames (two consecutive FLAGs with nothing between) are silently
// skipped; a stray ESC at end of stream returns ErrTruncatedEscape.
type HDLCDecoder struct {
	r *bufio.Reader
}

// ErrTruncatedEscape is returned if an ESC byte is followed immediately by
// EOF — the stream lost a byte.
var ErrTruncatedEscape = errors.New("hdlc: truncated escape sequence at end of stream")

func NewHDLCDecoder(r io.Reader) *HDLCDecoder {
	return &HDLCDecoder{r: bufio.NewReader(r)}
}

// NextFrame reads one HDLC frame and returns its un-escaped payload. The
// returned slice is freshly allocated. io.EOF is returned only on a clean
// stream end (no partial frame).
func (d *HDLCDecoder) NextFrame() ([]byte, error) {
	for {
		// Skip leading flags / junk until we have at least one non-flag byte.
		first, err := d.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if first == hdlcFlag {
			continue
		}

		out := []byte{}
		// Process the first byte (might be an ESC).
		b := first
		// oversized marks a frame that has exceeded maxHDLCFrameSize: we
		// stop buffering but keep scanning for the closing FLAG, so a
		// peer streaming an unterminated frame cannot OOM us (audit A5).
		// The oversized frame is then discarded and decoding resyncs to
		// the next frame.
		oversized := false
		for {
			if b == hdlcFlag {
				if oversized {
					break // discard the oversized frame, resync
				}
				return out, nil
			}
			if b == hdlcEsc {
				next, err := d.r.ReadByte()
				if err != nil {
					if errors.Is(err, io.EOF) {
						return nil, ErrTruncatedEscape
					}
					return nil, err
				}
				if !oversized {
					out = append(out, next^hdlcEscMask)
				}
			} else if !oversized {
				out = append(out, b)
			}
			if !oversized && len(out) > maxHDLCFrameSize {
				oversized = true
				out = nil
			}
			b, err = d.r.ReadByte()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil, ErrTruncatedEscape
				}
				return nil, err
			}
		}
	}
}
