package zstd

import "errors"

// bitReader reads bits out of a byte slice in the order zstd uses.
//
// Zstandard packs several independent bitstreams (Huffman literals, FSE
// sequences, Huffman weights) into one byte slice. Each of them is written
// "forward" by the compressor but must be consumed "backward" by the
// decompressor: the last byte holds up to 7 zero padding bits, then a single
// 1 bit, and the useful bits run backwards from there.
//
// Positions are addressed as a flat, little-endian bit index: bit 0 is the
// lowest bit of byte 0, bit 8 is the lowest bit of byte 1, and so on. Reading
// n bits at position p therefore yields the same value the compressor wrote
// as a little-endian field, which is what every value in the format means.
//
// A forward reader is used for a block's FSE distribution table (which is
// genuinely read forward) and a reverse reader for its bitstream.
type bitReader struct {
	buf []byte
	// pos is the position of the next bit to read.
	pos int
	// nbits is the total number of readable bits.
	nbits int
	// reverse means pos counts down and each field is assembled with the
	// field's own lowest bit nearest pos.
	reverse bool
}

var errBitEOF = errors.New("zstd: unexpected end of bitstream")

// newForwardBitReader reads bits starting at the lowest bit of buf[0].
func newForwardBitReader(buf []byte) *bitReader {
	return &bitReader{buf: buf, nbits: len(buf) * 8}
}

// newReverseBitReader starts at the highest bit before the padding sentinel.
//
// The last non-zero byte is located, its highest set bit is the sentinel and
// is skipped, and reading then proceeds downwards.
func newReverseBitReader(buf []byte) (*bitReader, error) {
	i := len(buf)
	for i > 0 && buf[i-1] == 0 {
		i--
	}
	if i == 0 {
		return nil, errors.New("zstd: bitstream is empty")
	}
	last := buf[i-1]
	// Bit index of the sentinel within the last byte.
	bit := 7
	for bit >= 0 && last&(1<<uint(bit)) == 0 {
		bit--
	}
	// Everything at or below `bit` in the trailing zero bytes is padding.
	total := (i-1)*8 + bit
	return &bitReader{buf: buf[:i], pos: total, nbits: total, reverse: true}, nil
}

// bitsLeft reports how many bits remain unread.
func (r *bitReader) bitsLeft() int { return r.pos }

// readForward returns the n-bit little-endian value at the current position.
func (r *bitReader) readForward(n int) (uint32, error) {
	if n == 0 {
		return 0, nil
	}
	if n > 32 || r.pos+n > r.nbits {
		return 0, errBitEOF
	}
	var v uint32
	for i := 0; i < n; i++ {
		p := r.pos + i
		bit := (r.buf[p>>3] >> uint(p&7)) & 1
		v |= uint32(bit) << uint(i)
	}
	r.pos += n
	return v, nil
}

// readReverse consumes n bits, walking downwards from the current position.
//
// The bit nearest the current position is the most significant bit of the
// returned value, which is how the compressor assembles fields when writing
// forward.
//
// A read that runs off the start of the stream yields zero bits rather than an
// error: the format explicitly allows a final state update to do so, and a
// single 1 bit past the data acts as padding.
func (r *bitReader) readReverse(n int) (uint32, error) {
	if n == 0 {
		return 0, nil
	}
	if n > 32 {
		return 0, errBitEOF
	}
	var v uint32
	for i := 0; i < n; i++ {
		p := r.pos - 1 - i
		var bit byte
		if p >= 0 {
			bit = (r.buf[p>>3] >> uint(p&7)) & 1
		}
		v = v<<1 | uint32(bit)
	}
	r.pos -= n
	if r.pos < 0 {
		r.pos = 0
	}
	return v, nil
}

// canReadReverse reports whether n bits remain before the start of the stream.
// A false result is the decoder's equivalent of "bitstream overflow".
func (r *bitReader) canReadReverse(n int) bool { return r.pos >= n }

// peekReverse looks at the next n bits without consuming them. Bits past the
// beginning of the stream read as zero: the format explicitly allows a final
// state update to run off the start of the bitstream.
func (r *bitReader) peekReverse(n int) uint32 {
	if n > 32 {
		n = 32
	}
	var v uint32
	for i := 0; i < n; i++ {
		p := r.pos - 1 - i
		var bit byte
		if p >= 0 {
			bit = (r.buf[p>>3] >> uint(p&7)) & 1
		}
		v = v<<1 | uint32(bit)
	}
	return v
}

// skipReverse advances the read position by n bits.
func (r *bitReader) skipReverse(n int) { r.pos -= n }
