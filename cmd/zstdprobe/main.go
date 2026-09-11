// Command zstdprobe decompresses zstd files and reports a SHA-256 of the
// result, so output can be diffed against a reference implementation.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/liyixuan201211/agentlog/internal/zstd"
)

func main() {
	backend := zstd.BackendAuto
	args := os.Args[1:]
	if len(args) > 0 && strings.HasPrefix(args[0], "--backend=") {
		switch strings.TrimPrefix(args[0], "--backend=") {
		case "go":
			backend = zstd.BackendGo
		case "zstd":
			backend = zstd.BackendExternal
		}
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: zstdprobe [--backend=auto|go|zstd] <file.zst>...")
		os.Exit(2)
	}
	status := 0
	for _, name := range args {
		raw, err := os.ReadFile(name)
		if err != nil {
			fmt.Printf("%s\tERROR\t%v\n", name, err)
			status = 1
			continue
		}
		out, err := zstd.Decompress(raw, backend)
		if err != nil {
			fmt.Printf("%s\tERROR\t%v\n", name, err)
			status = 1
			continue
		}
		sum := sha256.Sum256(out)
		fmt.Printf("%s\tOK\t%d\t%s\n", name, len(out), hex.EncodeToString(sum[:]))
	}
	os.Exit(status)
}
