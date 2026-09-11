package zstd

import (
	"errors"
	"fmt"
)

// fseTable is a decoded Finite State Entropy table.
//
// Every state carries the symbol it decodes plus the number of extra bits to
// read and the baseline to add to them, which together produce the next state.
type fseTable struct {
	accuracyLog uint
	symbol      []uint8
	numBits     []uint8
	baseline    []uint16
}

// fseState is a cursor into an fseTable.
type fseState struct {
	table *fseTable
	value uint32
}

func (s fseState) symbol() uint8 { return s.table.symbol[s.value] }

// update consumes the state's extra bits and moves to the next state.
func (s *fseState) update(r *bitReader) error {
	n := int(s.table.numBits[s.value])
	bits, err := r.readReverse(n)
	if err != nil {
		return err
	}
	s.value = uint32(s.table.baseline[s.value]) + bits
	return nil
}

// buildFSE builds a decoding table from a normalized probability
// distribution.
//
// norm holds (1 << accuracyLog) probability points in total, except that a
// value of -1 means "less than one": those symbols claim one cell each at the
// end of the table and reset the state on every use.
func buildFSE(norm []int32, accuracyLog uint) (*fseTable, error) {
	if accuracyLog < 5 || accuracyLog > 15 {
		return nil, fmt.Errorf("zstd: FSE accuracy log %d out of range", accuracyLog)
	}
	tableSize := 1 << accuracyLog
	t := &fseTable{
		accuracyLog: accuracyLog,
		symbol:      make([]uint8, tableSize),
		numBits:     make([]uint8, tableSize),
		baseline:    make([]uint16, tableSize),
	}

	// The table is filled from both ends: "less than one" symbols grow down
	// from the top, everything else is spread by a stride from the bottom.
	highThreshold := tableSize - 1
	for sym, p := range norm {
		if p == -1 {
			if highThreshold < 0 {
				return nil, errors.New("zstd: too many low-probability symbols")
			}
			t.symbol[highThreshold] = uint8(sym)
			t.numBits[highThreshold] = uint8(accuracyLog)
			t.baseline[highThreshold] = 0
			highThreshold--
		}
	}

	step := (tableSize >> 1) + (tableSize >> 3) + 3
	mask := tableSize - 1
	pos := 0
	for sym, p := range norm {
		if p <= 0 {
			continue
		}
		for i := int32(0); i < p; i++ {
			t.symbol[pos] = uint8(sym)
			pos = (pos + step) & mask
			for pos > highThreshold {
				pos = (pos + step) & mask
			}
		}
	}
	if pos != 0 {
		return nil, errors.New("zstd: FSE distribution does not fill the table")
	}

	// Derive Number_of_Bits and Baseline cell by cell.
	//
	// A symbol with probability p occupies p consecutive "next state" slots
	// starting at p. For slot n the next state needs tableLog - highbit(n)
	// bits, and the baseline is the offset that maps those bits into
	// [0, tableSize).
	nextState := make([]uint32, 256)
	for i, p := range norm {
		if p == -1 {
			nextState[i] = 1
		} else {
			nextState[i] = uint32(p)
		}
	}
	for u := 0; u < tableSize; u++ {
		sym := t.symbol[u]
		n := nextState[sym]
		nextState[sym] = n + 1
		nb := uint8(accuracyLog - highBit32(n))
		t.numBits[u] = nb
		t.baseline[u] = uint16((n << uint(nb)) - uint32(tableSize))
	}
	return t, nil
}

func log2(v uint) uint {
	var n uint
	for v > 1 {
		v >>= 1
		n++
	}
	return n
}

func highBit32(v uint32) uint {
	var n uint
	for v > 1 {
		v >>= 1
		n++
	}
	return n
}

// readFSETable decodes an FSE normalized-count header.
//
// This follows the reference decoder's FSE_readNCount: the stream is consumed
// as a sliding 32-bit little-endian window, and each symbol's probability uses
// either nbBits-1 or nbBits bits depending on where it falls relative to a
// threshold derived from what is still unallocated.
func readFSETable(buf []byte, maxSymbol int, maxAccuracy uint) (*fseTable, int, error) {
	src := buf
	if len(src) < 8 {
		// The reference pads short inputs with zeroes so its windowed reads
		// stay in range; do the same.
		padded := make([]byte, 8)
		copy(padded, src)
		src = padded
	}
	iend := len(src)
	ip := 0
	charnum := 0
	bitStream := le32(src[ip:])
	tableLog := uint(bitStream&0xF) + 5
	if tableLog > maxAccuracy {
		return nil, 0, fmt.Errorf("zstd: FSE accuracy log %d exceeds maximum %d", tableLog, maxAccuracy)
	}
	bitStream >>= 4
	bitCount := 4

	nbBits := int(tableLog)
	remaining := int32(1)<<uint(nbBits) + 1
	threshold := int32(1) << uint(nbBits)
	nbBits++

	norm := make([]int32, 0, maxSymbol+1)
	previous0 := false

	for {
		if previous0 {
			// Zero-probability symbols are run-length encoded in 2-bit
			// groups: each value of 3 means three more zeroes.
			repeats := countTrailingZeros32(^bitStream) >> 1
			for repeats >= 12 {
				charnum += 3 * repeats
				ip += 3
				if ip > iend {
					return nil, 0, errors.New("zstd: FSE zero run runs past the table")
				}
				bitStream = le32(src[ip:])
				repeats = countTrailingZeros32(^bitStream) >> 1
			}
			charnum += 3 * repeats
			bitStream >>= uint(2 * repeats)
			bitCount += 2 * repeats
			// The final group is not 3 and terminates the run.
			charnum += int(bitStream & 3)
			bitCount += 2
			if charnum > maxSymbol {
				return nil, 0, fmt.Errorf("zstd: FSE table describes more than %d symbols", maxSymbol+1)
			}
			for len(norm) < charnum {
				norm = append(norm, 0)
			}
			previous0 = false
		}

		max := (2*threshold - 1) - remaining
		var count int32
		if int32(bitStream&uint32(threshold-1)) < max {
			count = int32(bitStream & uint32(threshold-1))
			bitCount += nbBits - 1
		} else {
			count = int32(bitStream & uint32(2*threshold-1))
			if count >= threshold {
				count -= max
			}
			bitCount += nbBits
		}
		count-- // the encoded value is one higher than the probability
		if count >= 0 {
			remaining -= count
		} else {
			remaining += count
		}
		if len(norm) >= maxSymbol+1 {
			return nil, 0, fmt.Errorf("zstd: FSE table describes more than %d symbols", maxSymbol+1)
		}
		norm = append(norm, count)
		charnum++
		previous0 = count == 0

		if remaining < threshold {
			if remaining <= 1 {
				break
			}
			nbBits = int(highBit32(uint32(remaining))) + 1
			threshold = int32(1) << uint(nbBits-1)
		}
		if charnum >= maxSymbol+1 {
			break
		}

		ip += bitCount >> 3
		bitCount &= 7
		if ip > iend {
			return nil, 0, errors.New("zstd: FSE table runs past the block")
		}
		bitStream = le32(src[ip:]) >> uint(bitCount)
	}
	if remaining != 1 {
		return nil, 0, fmt.Errorf("zstd: FSE distribution leaves %d unallocated points", remaining-1)
	}
	consumed := ip + ((bitCount + 7) >> 3)
	if consumed > len(buf) {
		return nil, 0, errors.New("zstd: FSE table description runs past the block")
	}
	t, err := buildFSE(norm, tableLog)
	if err != nil {
		return nil, 0, err
	}
	return t, consumed, nil
}

// le32 reads a little-endian uint32, treating a short slice as zero padded.
func le32(b []byte) uint32 {
	var v uint32
	for i := 0; i < 4 && i < len(b); i++ {
		v |= uint32(b[i]) << uint(8*i)
	}
	return v
}

func countTrailingZeros32(v uint32) int {
	if v == 0 {
		return 32
	}
	n := 0
	for v&1 == 0 {
		v >>= 1
		n++
	}
	return n
}
