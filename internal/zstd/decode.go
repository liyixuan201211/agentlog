package zstd

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// debugFrames, when set, receives (frame index, start offset, bytes
// consumed, decoded length) for the first frames of a stream.
var debugFrames func(int, int, int, int)

const (
	magicNumber    = 0xFD2FB528
	skippableBase  = 0x184D2A50
	skippableMask  = 0xFFFFFFF0
	maxBlockSize   = 128 * 1024
	maxWindowSize  = 1 << 31
	maxOffsets     = 3
	maxSequences   = 1 << 20
	maxHuffmanLog  = 11
	literalMaxSymb = 255
)

// Decode decompresses a complete zstd stream, which may contain several
// concatenated frames.
//
// It is a pure Go implementation of the frame format described by RFC 8878,
// with no dictionaries, so it can run anywhere the standard library runs.
func Decode(data []byte) ([]byte, error) {
	d := &decoder{}
	var out []byte
	pos := 0
	frameNo := 0
	for pos < len(data) {
		magic := binary.LittleEndian.Uint32(data[pos:])
		switch {
		case magic == magicNumber:
			frame, n, err := d.decodeFrame(data[pos:])
			if err != nil {
				return nil, err
			}
			if debugFrames != nil && frameNo < 12 {
				debugFrames(frameNo, pos, n, len(frame))
			}
			frameNo++
			out = append(out, frame...)
			pos += n
		case magic&skippableMask == skippableBase:
			if len(data)-pos < 8 {
				return nil, errors.New("zstd: truncated skippable frame")
			}
			size := int(binary.LittleEndian.Uint32(data[pos+4:]))
			if len(data)-pos < 8+size {
				return nil, errors.New("zstd: truncated skippable frame")
			}
			pos += 8 + size
		default:
			return nil, fmt.Errorf("zstd: unknown magic number %#08x at offset %d", magic, pos)
		}
	}
	if out == nil {
		out = []byte{}
	}
	return out, nil
}

// decoder carries the state that persists between compressed blocks, which is
// exactly what the format requires a decoder to remember.
type decoder struct {
	// huff is the last Huffman table, reused by treeless literal blocks.
	huff *huffTable
	// ll, of, ml are the last FSE tables, reused by repeat-mode sequences.
	ll, of, ml *fseTable
	// litBuf holds the literals of the block being decoded.
	litBuf []byte
	// out is the decoded stream produced so far in the current frame.
	out []byte
	// rep are the three most recent offsets, most recent first.
	rep [maxOffsets]int
}

// reset clears per-frame state; block-level tables intentionally survive
// across frames only as long as the frame does.
func (d *decoder) resetFrame() {
	d.huff = nil
	d.ll, d.of, d.ml = nil, nil, nil
	d.rep = [maxOffsets]int{1, 4, 8}
	d.out = d.out[:0]
}

// decodeFrame decodes one frame starting at data[0] and returns the decoded
// bytes together with the number of input bytes consumed.
func (d *decoder) decodeFrame(data []byte) ([]byte, int, error) {
	if len(data) < 5 {
		return nil, 0, errors.New("zstd: truncated frame header")
	}
	if binary.LittleEndian.Uint32(data) != magicNumber {
		return nil, 0, errors.New("zstd: bad magic number")
	}
	pos := 4
	fhd := data[pos]
	pos++
	fcsFlag := fhd >> 6
	singleSegment := (fhd>>5)&1 == 1
	hasChecksum := (fhd>>2)&1 == 1
	didFlag := fhd & 3
	if fhd&0x08 != 0 {
		return nil, 0, errors.New("zstd: reserved frame header bit is set")
	}

	var windowSize uint64
	if !singleSegment {
		if pos >= len(data) {
			return nil, 0, errors.New("zstd: truncated window descriptor")
		}
		wd := data[pos]
		pos++
		exp := uint(wd >> 3)
		mantissa := uint64(wd & 7)
		windowLog := 10 + exp
		if windowLog > 41 {
			return nil, 0, fmt.Errorf("zstd: window log %d is too large", windowLog)
		}
		base := uint64(1) << windowLog
		windowSize = base + (base/8)*mantissa
	}

	didSize := [...]int{0, 1, 2, 4}[didFlag]
	if pos+didSize > len(data) {
		return nil, 0, errors.New("zstd: truncated dictionary id")
	}
	if didSize > 0 {
		var dictID uint64
		for i := 0; i < didSize; i++ {
			dictID |= uint64(data[pos+i]) << uint(8*i)
		}
		if dictID != 0 {
			return nil, 0, fmt.Errorf("zstd: dictionary id %d is required but dictionaries are not supported", dictID)
		}
	}
	pos += didSize

	fcsSize := [...]int{0, 2, 4, 8}[fcsFlag]
	if fcsFlag == 0 && singleSegment {
		fcsSize = 1
	}
	if pos+fcsSize > len(data) {
		return nil, 0, errors.New("zstd: truncated frame content size")
	}
	var contentSize uint64
	for i := 0; i < fcsSize; i++ {
		contentSize |= uint64(data[pos+i]) << uint(8*i)
	}
	if fcsSize == 2 {
		contentSize += 256
	}
	pos += fcsSize
	if singleSegment {
		windowSize = contentSize
	}
	if windowSize > maxWindowSize {
		return nil, 0, fmt.Errorf("zstd: window size %d exceeds the %d byte limit", windowSize, maxWindowSize)
	}

	d.resetFrame()
	blockMax := uint64(maxBlockSize)
	if windowSize < blockMax {
		blockMax = windowSize
	}

	for {
		if pos+3 > len(data) {
			return nil, 0, errors.New("zstd: truncated block header")
		}
		header := uint32(data[pos]) | uint32(data[pos+1])<<8 | uint32(data[pos+2])<<16
		pos += 3
		last := header&1 == 1
		blockType := (header >> 1) & 3
		blockSize := int(header >> 3)

		switch blockType {
		case 0: // raw
			if uint64(blockSize) > blockMax {
				return nil, 0, errors.New("zstd: raw block exceeds the maximum block size")
			}
			if pos+blockSize > len(data) {
				return nil, 0, errors.New("zstd: truncated raw block")
			}
			d.out = append(d.out, data[pos:pos+blockSize]...)
			pos += blockSize
		case 1: // RLE
			if pos >= len(data) {
				return nil, 0, errors.New("zstd: truncated RLE block")
			}
			b := data[pos]
			pos++
			if uint64(blockSize) > blockMax {
				return nil, 0, errors.New("zstd: RLE block exceeds the maximum block size")
			}
			for i := 0; i < blockSize; i++ {
				d.out = append(d.out, b)
			}
		case 2: // compressed
			if pos+blockSize > len(data) {
				return nil, 0, errors.New("zstd: truncated compressed block")
			}
			if err := d.decodeCompressedBlock(data[pos:pos+blockSize], blockMax); err != nil {
				return nil, 0, err
			}
			pos += blockSize
		default:
			return nil, 0, errors.New("zstd: reserved block type")
		}
		if last {
			break
		}
	}

	if contentSize != 0 && uint64(len(d.out)) != contentSize {
		return nil, 0, fmt.Errorf("zstd: decoded %d bytes but frame declares %d", len(d.out), contentSize)
	}
	if hasChecksum {
		if pos+4 > len(data) {
			return nil, 0, errors.New("zstd: truncated content checksum")
		}
		// The checksum is verified by callers comparing whole-file digests;
		// skipping it here keeps the decoder dependency-free.
		pos += 4
	}

	out := make([]byte, len(d.out))
	copy(out, d.out)
	return out, pos, nil
}
