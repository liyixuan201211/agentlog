package zstd

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// ErrUnsupported reports that the pure-Go decoder met a construct it does not
// implement yet.
var ErrUnsupported = errors.New("zstd: unsupported construct")

// Backend selects how bytes are decompressed.
type Backend int

const (
	// BackendAuto uses the pure-Go decoder and falls back to an external
	// zstd binary when one is installed.
	BackendAuto Backend = iota
	// BackendGo forces the pure-Go decoder.
	BackendGo
	// BackendExternal forces the external zstd binary.
	BackendExternal
)

var (
	lookOnce sync.Once
	lookPath string
)

// externalZstd returns the path of a usable zstd binary, or "".
//
// PATH is consulted first; a few conventional locations are checked as well,
// because GUI and test processes often run with a minimal environment.
func externalZstd() string {
	lookOnce.Do(func() {
		lookPath = findZstd()
	})
	return lookPath
}

// findZstd locates a usable zstd binary, or returns "".
func findZstd() string {
	seen := map[string]bool{}
	check := func(p string) string {
		if p == "" || seen[p] {
			return ""
		}
		seen[p] = true
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			return ""
		}
		// The reference CLI prints "*** Zstandard CLI ...", with a capital Z.
		out, err := exec.Command(p, "--version").Output()
		if err != nil || !bytes.Contains(out, []byte("Zstandard")) {
			return ""
		}
		return p
	}

	if p, err := exec.LookPath("zstd"); err == nil {
		if got := check(p); got != "" {
			return got
		}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if got := check(filepath.Join(dir, "zstd")); got != "" {
			return got
		}
	}
	for _, home := range []string{os.Getenv("HOME"), "/Users", "/root"} {
		if home == "" {
			continue
		}
		for _, rel := range []string{".local/bin/zstd", "bin/zstd"} {
			if got := check(filepath.Join(home, rel)); got != "" {
				return got
			}
		}
	}
	for _, p := range []string{"/opt/homebrew/bin/zstd", "/usr/local/bin/zstd", "/usr/bin/zstd", "/bin/zstd"} {
		if got := check(p); got != "" {
			return got
		}
	}
	return ""
}

// AvailableExternal reports whether an external zstd binary was found.
func AvailableExternal() bool { return externalZstd() != "" }

// Decompress decodes a zstd stream using the requested backend.
//
// The pure-Go decoder implements the frame format directly; for streams that
// use constructs it does not cover yet, BackendAuto delegates to an installed
// zstd binary so no data is silently lost.
func Decompress(data []byte, backend Backend) ([]byte, error) {
	switch backend {
	case BackendGo:
		return Decode(data)
	case BackendExternal:
		return decodeExternal(data)
	default:
		out, err := Decode(data)
		if err == nil {
			return out, nil
		}
		// Only delegate for constructs the pure-Go decoder genuinely does not
		// implement; a real corruption error must surface as such.
		if !errors.Is(err, ErrUnsupported) || externalZstd() == "" {
			return nil, err
		}
		alt, altErr := decodeExternal(data)
		if altErr != nil {
			return nil, err
		}
		return alt, nil
	}
}

func decodeExternal(data []byte) ([]byte, error) {
	bin := externalZstd()
	if bin == "" {
		return nil, errors.New("zstd: no external zstd binary found in PATH")
	}
	cmd := exec.Command(bin, "-dc", "--no-progress", "-")
	cmd.Stdin = bytes.NewReader(data)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, errors.New("zstd: external decoder failed: " + errBuf.String())
	}
	return out.Bytes(), nil
}
