package zstd

import (
	"errors"
	"fmt"
)

// huffTable is a flat prefix-code lookup table.
//
// The literal bitstream is read backwards, which reconstructs the codes in the
// order the compressor wrote them, so a table indexed by the next maxBits bits
// read in that direction decodes one symbol at a time.
type huffTable struct {
	maxBits int
	symbol  []uint8
	length  []uint8
}

// readHuffmanTree parses a Huffman tree description and returns the decoding
// table plus the number of bytes consumed.
func readHuffmanTree(buf []byte) (*huffTable, int, error) {
	if len(buf) == 0 {
		return nil, 0, errors.New("zstd: empty Huffman tree description")
	}
	header := buf[0]
	var weights []uint8
	var consumed int

	if header >= 128 {
		// Direct representation: 4 bits per weight, two per byte, high nibble
		// first. The series stops at the last non-zero weight, so the number
		// of bytes follows the values rather than the symbol count.
		weightsEnd := int(header) - 127
		o := 0
		n := weightsEnd
		for o*2 < n {
			if 1+o >= len(buf) {
				return nil, 0, errors.New("zstd: truncated Huffman weights")
			}
			b := buf[1+o]
			o++
			for _, w := range [2]uint8{b >> 4, b & 0x0f} {
				if w == 0 {
					n--
				} else {
					// Position of the next weight follows from this value.
					n = o*2 - 2 + int(w)
				}
			}
		}
		if 1+o > len(buf) {
			return nil, 0, errors.New("zstd: truncated Huffman weights")
		}
		last := buf[o]
		weights = make([]uint8, 0, weightsEnd)
		for i := 0; i < o-1; i++ {
			weights = append(weights, buf[1+i]>>4, buf[1+i]&0x0f)
		}
		weights = append(weights, last>>4)
		if last&0x0f > 0 {
			weights = append(weights, last&0x0f)
		}
		consumed = 1 + o
	} else {
		// FSE-compressed weights are not implemented correctly by the pure-Go
		// decoder yet. Reporting this distinctly lets the caller delegate to
		// an external zstd binary rather than return wrong bytes; a silent
		// mis-decode is worse than a slower correct one.
		return nil, 0, ErrUnsupported
	}

	t, err := buildHuffTable(weights)
	if err != nil {
		return nil, 0, err
	}
	return t, consumed, nil
}

// buildHuffTable converts a weight series into a decoding table.
//
// weights describes every symbol but the last: the transmitted series is
// already a complete tree, so it sums to a power of two, which is the tree
// depth. An implied final symbol of weight 1 is then appended.
func buildHuffTable(weights []uint8) (*huffTable, error) {
	if len(weights) == 0 {
		return nil, errors.New("zstd: no Huffman weights")
	}
	if len(weights) > 255 {
		return nil, errors.New("zstd: too many Huffman weights")
	}
	var total uint32
	for _, w := range weights {
		if w > 11 {
			return nil, fmt.Errorf("zstd: Huffman weight %d exceeds the 11-bit limit", w)
		}
		if w > 0 {
			total += 1 << (w - 1)
		}
	}
	if total == 0 {
		return nil, errors.New("zstd: Huffman tree has no symbols")
	}
	// The tree depth is the smallest power of two strictly greater than the
	// transmitted weight sum; the leftover fills in the implied final symbol.
	maxBits := int(log2ceil(total))
	if total == uint32(1)<<uint(maxBits) {
		maxBits++
	}
	if maxBits > 11 {
		return nil, fmt.Errorf("zstd: Huffman tree depth %d exceeds 11", maxBits)
	}
	totalSpace := uint32(1) << uint(maxBits)
	rest := totalSpace - total
	if rest == 0 || rest&(rest-1) != 0 {
		return nil, errors.New("zstd: Huffman weights do not form a complete tree")
	}
	lastWeight := int(log2uint(rest)) + 1

	// The implied final symbol takes the single remaining slot.
	all := make([]uint8, len(weights), len(weights)+1)
	copy(all, weights)
	all = append(all, uint8(lastWeight))

	// Canonical codes: ascending weight, then natural symbol order.
	//
	// code tracks the running position in code space; the canonical code for a
	// symbol is that position divided by 2^(maxBits - length), which is the
	// same value shifted down by the number of bits it occupies.
	var codes []uint32
	var lengths []uint8
	var syms []int
	code := uint32(0)
	for w := 1; w <= maxBits; w++ {
		for sym, sw := range all {
			if int(sw) != w {
				continue
			}
			length := maxBits + 1 - w
			codes = append(codes, code>>uint(maxBits-length))
			lengths = append(lengths, uint8(length))
			syms = append(syms, sym)
			code += 1 << uint(maxBits-length)
		}
	}
	if code != 1<<uint(maxBits) {
		return nil, errors.New("zstd: Huffman code space is not fully used")
	}

	t := &huffTable{
		maxBits: maxBits,
		symbol:  make([]uint8, 1<<uint(maxBits)),
		length:  make([]uint8, 1<<uint(maxBits)),
	}
	for i, sym := range syms {
		l := int(lengths[i])
		shift := uint(maxBits - l)
		base := int(codes[i]) << shift
		for j := 0; j < 1<<shift; j++ {
			t.symbol[base+j] = uint8(sym)
			t.length[base+j] = uint8(l)
		}
	}
	return t, nil
}

func log2ceil(v uint32) uint {
	var n uint
	for (uint32(1) << n) < v {
		n++
	}
	return n
}

// decodeHuffmanStream decodes exactly n symbols from a Huffman bitstream.
func decodeHuffmanStream(buf []byte, t *huffTable, n int, dst []byte) error {
	if n == 0 {
		return nil
	}
	r, err := newReverseBitReader(buf)
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		peek := r.peekReverse(t.maxBits)
		sym := t.symbol[peek]
		l := int(t.length[peek])
		if l == 0 || l > r.bitsLeft() {
			return errors.New("zstd: corrupt Huffman stream")
		}
		dst[i] = sym
		r.skipReverse(l)
	}
	return nil
}

// bitLen32 returns the minimum number of bits needed to represent v, with
// bitLen32(0) == 0.
func bitLen32(v uint32) int {
	n := 0
	for v > 0 {
		v >>= 1
		n++
	}
	return n
}

// log2uint returns floor(log2(v)) for v >= 1.
func log2uint(v uint32) uint {
	var n uint
	for v > 1 {
		v >>= 1
		n++
	}
	return n
}
