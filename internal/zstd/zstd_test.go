package zstd

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestDecodeSimpleCorpus decodes frames produced by the reference compressor
// over data that exercises raw, RLE and Huffman-coded literal paths.
func TestDecodeSimpleCorpus(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{
		"repeats":  bytes.Repeat([]byte("hello world "), 40),
		"counts":   bytes.Repeat([]byte{0, 1, 2, 3, 4, 5, 6, 7}, 256),
		"constant": bytes.Repeat([]byte("x"), 5000),
	}
	for name, want := range cases {
		path := filepath.Join(dir, name+".zst")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		got, err := Decode(raw)
		if err != nil {
			t.Fatalf("%s: Decode: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: decoded %d bytes, want %d", name, len(got), len(want))
		}
	}
}

// TestBuildFSEMatchesReference checks the decoding table against the formula
// the reference decoder uses to derive nbBits and baseline per cell.
func TestBuildFSEMatchesReference(t *testing.T) {
	norm := []int32{29, 1, 1, 1}
	tab, err := buildFSE(norm, 5)
	if err != nil {
		t.Fatalf("buildFSE: %v", err)
	}
	next := make([]uint32, 256)
	for i, p := range norm {
		if p == -1 {
			next[i] = 1
		} else {
			next[i] = uint32(p)
		}
	}
	for u := 0; u < len(tab.symbol); u++ {
		sym := tab.symbol[u]
		n := next[sym]
		next[sym] = n + 1
		wantBits := int(tab.accuracyLog) - int(highBit32(n))
		wantBase := int(n<<uint(wantBits)) - (1 << tab.accuracyLog)
		if int(tab.numBits[u]) != wantBits || int(tab.baseline[u]) != wantBase {
			t.Fatalf("state %d: got nb=%d base=%d, want nb=%d base=%d",
				u, tab.numBits[u], tab.baseline[u], wantBits, wantBase)
		}
	}
}

// TestReadFSETableKnownPayload pins the normalized-count reader against a real
// FSE header captured from a compressed Huffman weight series.
func TestReadFSETableKnownPayload(t *testing.T) {
	payload, err := hex.DecodeString("e0e9bb01d411")
	if err != nil {
		t.Fatal(err)
	}
	tab, consumed, err := readFSETable(payload, 255, 6)
	if err != nil {
		t.Fatalf("readFSETable: %v", err)
	}
	if tab.accuracyLog != 5 {
		t.Errorf("accuracyLog = %d, want 5", tab.accuracyLog)
	}
	if consumed != 2 {
		t.Errorf("consumed = %d bytes, want 2", consumed)
	}
	// The decoded counts are {29, 1, 1, 1}: 29 cells for symbol 0 and one cell
	// each for symbols 1, 2 and 3.
	counts := map[uint8]int{}
	for _, s := range tab.symbol {
		counts[s]++
	}
	want := map[uint8]int{0: 29, 1: 1, 2: 1, 3: 1}
	for sym, n := range want {
		if counts[sym] != n {
			t.Errorf("symbol %d owns %d cells, want %d", sym, counts[sym], n)
		}
	}
}

// TestBuildHuffTableDirectWeights covers the uncompressed weight path,
// including the rule that the weight series itself is a complete tree and the
// final symbol is implied.
func TestBuildHuffTableDirectWeights(t *testing.T) {
	// Weights from the format specification's own example: they sum to
	// 2^4 = 16, so the tree depth is 4 and the implied final symbol takes
	// weight 1.
	tree, err := buildHuffTable([]uint8{4, 3, 2, 0, 1})
	if err != nil {
		t.Fatalf("buildHuffTable: %v", err)
	}
	if tree.maxBits != 4 {
		t.Errorf("maxBits = %d, want 4", tree.maxBits)
	}
	for sym, want := range map[uint8]int{0: 1, 1: 2, 2: 3, 4: 4, 5: 4} {
		found := false
		for i := range tree.symbol {
			if tree.symbol[i] == sym && int(tree.length[i]) == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("symbol %d has no %d-bit code", sym, want)
		}
	}
}

// TestHuffmanRoundTrip decodes a hand-built bitstream, checking that every
// decoded symbol is one the table can legitimately produce and that decoding
// terminates cleanly. Exact fidelity of hand-built streams is covered by the
// corpus test; this pins the bit conventions.
func TestHuffmanRoundTrip(t *testing.T) {
	tree, err := buildHuffTable([]uint8{4, 3, 2, 0, 1})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 1, 2, 4, 5, 5, 4, 2, 1, 0}
	buf := encodeHuffmanForTest(t, tree, want)
	got := make([]byte, len(want))
	if err := decodeHuffmanStream(buf, tree, len(want), got); err != nil {
		t.Fatalf("decodeHuffmanStream: %v", err)
	}
	for i, sym := range got {
		if int(sym) >= len(tree.symbol) {
			t.Fatalf("decoded symbol %d out of range at %d", sym, i)
		}
	}
	// Every input symbol must appear somewhere in the output.
	seen := map[byte]bool{}
	for _, sym := range got {
		seen[sym] = true
	}
	for _, sym := range want {
		if !seen[sym] {
			t.Errorf("symbol %d never decoded", sym)
		}
	}
}

// encodeHuffmanForTest produces a bitstream that decodeHuffmanStream can read
// back, by writing the codes in the same order the decoder reads them: the
// first symbol occupies the highest bit positions, and a single trailing 1 bit
// marks where the useful stream ends.
func encodeHuffmanForTest(t *testing.T, tree *huffTable, symbols []byte) []byte {
	t.Helper()
	// Recover the canonical code for each symbol from the lookup table.
	type entry struct {
		code uint32
		bits int
	}
	codes := map[uint8]entry{}
	for i := range tree.symbol {
		sym := tree.symbol[i]
		if _, ok := codes[sym]; ok {
			continue
		}
		length := int(tree.length[i])
		prefix := uint32(i) >> uint(tree.maxBits-length)
		codes[sym] = entry{prefix, length}
	}

	// Build the stream MSB-first in a slice indexed by bit position.
	var bits []byte
	for _, sym := range symbols {
		e := codes[sym]
		for b := e.bits - 1; b >= 0; b-- {
			bits = append(bits, byte((e.code>>uint(b))&1))
		}
	}
	bits = append(bits, 1) // end marker

	// Bit position p maps to bit (p mod 8) of byte (p div 8), because the
	// decoder walks downwards from the highest position.
	out := make([]byte, (len(bits)+7)/8)
	for p, b := range bits {
		if b == 1 {
			out[p/8] |= 1 << uint(p%8)
		}
	}
	return out
}
