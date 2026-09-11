package zstd

import (
	"errors"
	"fmt"
)

// decodeCompressedBlock decodes one compressed block and appends its output
// to d.out.
func (d *decoder) decodeCompressedBlock(data []byte, blockMax uint64) error {
	literals, consumed, err := d.readLiterals(data)
	if err != nil {
		return err
	}
	seq := data[consumed:]
	if len(seq) == 0 {
		return errors.New("zstd: compressed block has no sequences section")
	}

	// The sequences section header: a variable-length count followed by one
	// byte of per-symbol compression modes.
	pos := 0
	b0 := seq[pos]
	pos++
	var numSequences int
	switch {
	case b0 == 0:
		// No sequences: the block is entirely literals, and the FSE tables
		// used by repeat mode are explicitly left untouched.
		d.out = append(d.out, literals...)
		return nil
	case b0 < 128:
		numSequences = int(b0)
	case b0 < 255:
		if pos >= len(seq) {
			return errors.New("zstd: truncated sequence count")
		}
		numSequences = (int(b0)-128)<<8 + int(seq[pos])
		pos++
	default:
		if pos+2 > len(seq) {
			return errors.New("zstd: truncated sequence count")
		}
		numSequences = int(seq[pos]) + int(seq[pos+1])<<8 + 0x7F00
		pos += 2
	}
	if numSequences > maxSequences {
		return fmt.Errorf("zstd: %d sequences in one block is implausible", numSequences)
	}
	if pos >= len(seq) {
		return errors.New("zstd: missing symbol compression modes")
	}
	modes := seq[pos]
	pos++
	llMode := (modes >> 6) & 3
	ofMode := (modes >> 4) & 3
	mlMode := (modes >> 2) & 3
	if modes&3 != 0 {
		return errors.New("zstd: reserved sequence mode bits are set")
	}

	llTable, n, err := d.resolveTable(llMode, "literals length", 35, 9, llDefaultNorm, llDefaultAccuracy, &d.ll, seq[pos:])
	if err != nil {
		return err
	}
	pos += n
	ofTable, n, err := d.resolveTable(ofMode, "offset", 31, 8, ofDefaultNorm, ofDefaultAccuracy, &d.of, seq[pos:])
	if err != nil {
		return err
	}
	pos += n
	mlTable, n, err := d.resolveTable(mlMode, "match length", 52, 9, mlDefaultNorm, mlDefaultAccuracy, &d.ml, seq[pos:])
	if err != nil {
		return err
	}
	pos += n

	if pos > len(seq) {
		return errors.New("zstd: sequence tables run past the block")
	}
	return d.executeSequences(seq[pos:], numSequences, literals, llTable, ofTable, mlTable)
}

// readLiterals parses the literals section and returns the decoded literals
// together with the number of block bytes the section occupied.
func (d *decoder) readLiterals(data []byte) ([]byte, int, error) {
	if len(data) == 0 {
		return nil, 0, errors.New("zstd: empty compressed block")
	}
	b0 := data[0]
	litType := b0 & 3

	switch litType {
	case 0: // Raw_Literals_Block
		size, header := rawLiteralSize(data)
		if header == 0 || header+size > len(data) {
			return nil, 0, errors.New("zstd: truncated raw literals")
		}
		return data[header : header+size], header + size, nil

	case 1: // RLE_Literals_Block
		size, header := rawLiteralSize(data)
		if header == 0 || header >= len(data) {
			return nil, 0, errors.New("zstd: truncated RLE literals")
		}
		b := data[header]
		out := make([]byte, size)
		for i := range out {
			out[i] = b
		}
		return out, header + 1, nil

	default: // Compressed_Literals_Block or Treeless_Literals_Block
		return d.readCompressedLiterals(data, litType)
	}
}

// rawLiteralSize decodes the literals header shared by the raw and RLE block
// types, returning the regenerated size and the header length.
func rawLiteralSize(data []byte) (int, int) {
	sizeFormat := (data[0] >> 2) & 3
	switch sizeFormat {
	case 0, 2:
		if len(data) < 1 {
			return 0, 0
		}
		return int(data[0] >> 3), 1
	case 1:
		if len(data) < 2 {
			return 0, 0
		}
		return int(data[0]>>4) + int(data[1])<<4, 2
	default:
		if len(data) < 3 {
			return 0, 0
		}
		return int(data[0]>>4) + int(data[1])<<4 + int(data[2])<<12, 3
	}
}

// readCompressedLiterals decodes a Huffman-coded literals section.
func (d *decoder) readCompressedLiterals(data []byte, litType byte) ([]byte, int, error) {
	sizeFormat := (data[0] >> 2) & 3
	if len(data) < 3 {
		return nil, 0, errors.New("zstd: truncated literals header")
	}

	// Both compressed variants use a 2-bit format selecting how wide the two
	// size fields are, and how many streams follow.
	var regenSize, compSize, headerLen, streams int
	switch sizeFormat {
	case 0:
		v := uint32(data[1]) | uint32(data[2])<<8
		regenSize = int(v & 0x3FF)
		compSize = int((v >> 10) & 0x3FF)
		headerLen, streams = 3, 1
	case 1:
		v := uint32(data[1]) | uint32(data[2])<<8
		regenSize = int(v & 0x3FF)
		compSize = int((v >> 10) & 0x3FF)
		headerLen, streams = 3, 4
	case 2:
		if len(data) < 4 {
			return nil, 0, errors.New("zstd: truncated literals header")
		}
		v := uint32(data[1]) | uint32(data[2])<<8 | uint32(data[3])<<16
		regenSize = int(v & 0x3FFF)
		compSize = int((v >> 14) & 0x3FFF)
		headerLen, streams = 4, 4
	default:
		if len(data) < 5 {
			return nil, 0, errors.New("zstd: truncated literals header")
		}
		v := uint32(data[1]) | uint32(data[2])<<8 | uint32(data[3])<<16 | uint32(data[4])<<24
		regenSize = int(v & 0x3FFFF)
		compSize = int((v >> 18) & 0x3FFFF)
		headerLen, streams = 5, 4
	}

	body := data[headerLen:]
	if compSize > len(body) {
		return nil, 0, errors.New("zstd: literals section runs past the block")
	}
	body = body[:compSize]

	if litType == 2 {
		// A new tree description precedes the streams.
		tree, n, err := readHuffmanTree(body)
		if err != nil {
			return nil, 0, err
		}
		d.huff = tree
		body = body[n:]
	} else if d.huff == nil {
		return nil, 0, errors.New("zstd: treeless literals without a previous Huffman table")
	}

	if regenSize > maxBlockSize {
		return nil, 0, errors.New("zstd: literals section exceeds the maximum block size")
	}
	out := make([]byte, regenSize)

	if streams == 1 {
		if err := decodeHuffmanStream(body, d.huff, regenSize, out); err != nil {
			return nil, 0, err
		}
		return out, headerLen + compSize, nil
	}

	// Four streams, preceded by a 6-byte jump table giving the first three
	// compressed sizes.
	if len(body) < 6 {
		return nil, 0, errors.New("zstd: missing jump table")
	}
	s1 := int(uint16(body[0]) | uint16(body[1])<<8)
	s2 := int(uint16(body[2]) | uint16(body[3])<<8)
	s3 := int(uint16(body[4]) | uint16(body[5])<<8)
	streamsData := body[6:]
	s4 := len(streamsData) - s1 - s2 - s3
	if s4 < 0 {
		return nil, 0, errors.New("zstd: jump table sizes exceed the literals section")
	}
	// Each stream decodes a quarter of the output; the last takes the
	// remainder.
	per := (regenSize + 3) / 4
	sizes := [4]int{per, per, per, regenSize - 3*per}
	chunks := [4][]byte{
		streamsData[:s1],
		streamsData[s1 : s1+s2],
		streamsData[s1+s2 : s1+s2+s3],
		streamsData[s1+s2+s3 : s1+s2+s3+s4],
	}
	off := 0
	for i := 0; i < 4; i++ {
		if err := decodeHuffmanStream(chunks[i], d.huff, sizes[i], out[off:]); err != nil {
			return nil, 0, err
		}
		off += sizes[i]
	}
	return out, headerLen + compSize, nil
}

// resolveTable returns the FSE table selected by a compression mode, reading
// and remembering a new description when necessary.
func (d *decoder) resolveTable(mode byte, name string, maxSymbol int, maxAccuracy uint, def []int32, defLog uint, slot **fseTable, buf []byte) (*fseTable, int, error) {
	switch mode {
	case 0: // Predefined_Mode
		t, err := buildFSE(def, defLog)
		if err != nil {
			return nil, 0, fmt.Errorf("zstd: predefined %s table: %w", name, err)
		}
		*slot = t
		return t, 0, nil
	case 1: // RLE_Mode: one transmitted byte is the only symbol
		if len(buf) < 1 {
			return nil, 0, fmt.Errorf("zstd: missing RLE symbol for %s", name)
		}
		t, err := buildRLEFSE(buf[0], maxAccuracy)
		if err != nil {
			return nil, 0, err
		}
		*slot = t
		return t, 1, nil
	case 2: // FSE_Compressed_Mode
		t, n, err := readFSETable(buf, maxSymbol, maxAccuracy)
		if err != nil {
			return nil, 0, fmt.Errorf("zstd: %s table: %w", name, err)
		}
		*slot = t
		return t, n, nil
	default: // Repeat_Mode
		if *slot == nil {
			return nil, 0, fmt.Errorf("zstd: %s table repeated before being defined", name)
		}
		return *slot, 0, nil
	}
}

// buildRLEFSE builds a degenerate table whose only symbol never consumes bits.
func buildRLEFSE(symbol byte, accuracyLog uint) (*fseTable, error) {
	size := 1 << accuracyLog
	t := &fseTable{
		accuracyLog: accuracyLog,
		symbol:      make([]uint8, size),
		numBits:     make([]uint8, size),
		baseline:    make([]uint16, size),
	}
	for i := range t.symbol {
		t.symbol[i] = symbol
	}
	return t, nil
}

// executeSequences decodes the interleaved sequence bitstream and runs the
// resulting copy commands.
func (d *decoder) executeSequences(bitstream []byte, numSequences int, literals []byte, ll, of, ml *fseTable) error {
	if numSequences == 0 {
		d.out = append(d.out, literals...)
		return nil
	}
	r, err := newReverseBitReader(bitstream)
	if err != nil {
		return err
	}

	llState := fseState{table: ll}
	ofState := fseState{table: of}
	mlState := fseState{table: ml}
	// Initial states are read in this order.
	if llState.value, err = r.readReverse(int(ll.accuracyLog)); err != nil {
		return err
	}
	if ofState.value, err = r.readReverse(int(of.accuracyLog)); err != nil {
		return err
	}
	if mlState.value, err = r.readReverse(int(ml.accuracyLog)); err != nil {
		return err
	}

	litPos := 0
	for i := 0; i < numSequences; i++ {
		// Per sequence the decoder reads offset bits, then match length, then
		// literals length.
		ofCode := ofState.symbol()
		ofExtra, err := r.readReverse(int(ofCode))
		if err != nil {
			return err
		}
		offsetValue := int(1)<<ofCode + int(ofExtra)

		mlCode := mlState.symbol()
		mlExtra, err := r.readReverse(int(mlBits[mlCode]))
		if err != nil {
			return err
		}
		matchLen := mlBase[mlCode] + int32(mlExtra)

		llCode := llState.symbol()
		llExtra, err := r.readReverse(int(llBits[llCode]))
		if err != nil {
			return err
		}
		litLen := llBase[llCode] + int32(llExtra)

		if litPos+int(litLen) > len(literals) {
			return errors.New("zstd: sequence asks for more literals than the block holds")
		}
		if litLen > 0 {
			d.out = append(d.out, literals[litPos:litPos+int(litLen)]...)
			litPos += int(litLen)
		}

		offset, err := d.resolveOffset(offsetValue, litLen)
		if err != nil {
			return err
		}
		if offset <= 0 || offset > len(d.out) {
			return fmt.Errorf("zstd: match offset %d is out of range", offset)
		}
		start := len(d.out) - offset
		for n := int32(0); n < matchLen; n++ {
			d.out = append(d.out, d.out[start+int(n)])
		}
		d.updateRepeatOffsets(offsetValue, litLen, offset)

		if i == numSequences-1 {
			break
		}
		// State updates run in this order.
		if err := llState.update(r); err != nil {
			return err
		}
		if err := mlState.update(r); err != nil {
			return err
		}
		if err := ofState.update(r); err != nil {
			return err
		}
	}

	// Leftover literals are appended verbatim.
	if litPos < len(literals) {
		d.out = append(d.out, literals[litPos:]...)
	}
	return nil
}

// resolveOffset turns a decoded offset value into a real back-reference
// distance, resolving the three repeat codes.
func (d *decoder) resolveOffset(offsetValue int, litLen int32) (int, error) {
	if offsetValue > 3 {
		return offsetValue - 3, nil
	}
	if litLen == 0 {
		// With no literals the repeat codes shift by one.
		switch offsetValue {
		case 1:
			return d.rep[1], nil
		case 2:
			return d.rep[2], nil
		case 3:
			return d.rep[0] - 1, nil
		}
	} else {
		switch offsetValue {
		case 1:
			return d.rep[0], nil
		case 2:
			return d.rep[1], nil
		case 3:
			return d.rep[2], nil
		}
	}
	return 0, fmt.Errorf("zstd: invalid offset value %d", offsetValue)
}

// updateRepeatOffsets rotates the recent-offset history after a sequence.
//
// Without literals, offset value 3 means "Repeated_Offset1 minus one" and is
// therefore a fresh offset rather than a repeat.
func (d *decoder) updateRepeatOffsets(offsetValue int, litLen int32, offset int) {
	if offsetValue > 3 || (offsetValue == 3 && litLen == 0) {
		d.rep[2] = d.rep[1]
		d.rep[1] = d.rep[0]
		d.rep[0] = offset
		return
	}
	idx := offsetValue - 1 // 0, 1 or 2
	if litLen == 0 {
		idx-- // repeat codes shifted by one
	}
	if idx <= 0 {
		return
	}
	used := d.rep[idx]
	for i := idx; i > 0; i-- {
		d.rep[i] = d.rep[i-1]
	}
	d.rep[0] = used
}
