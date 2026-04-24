package paths

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"zed-remote-wrapper/internal/protocol"
)

// ParsePathSpec turns a user-provided path (possibly with `~`, relative,
// or `path:line:col`) into an absolute PathSpec. When a file exists at
// the literal spelling, its `:L:C` suffix (if any) is NOT split, so that
// paths legitimately containing colons are preserved.
func ParsePathSpec(raw, cwd, home string) (protocol.PathSpec, error) {
	if raw == "" {
		return protocol.PathSpec{}, nil
	}

	literal := expand(raw, home)
	literalAbs := abs(literal, cwd)
	if _, err := os.Stat(literalAbs); err == nil {
		return protocol.PathSpec{Path: literalAbs}, nil
	}

	path, line, col := splitLineCol(raw)
	path = expand(path, home)
	path = abs(path, cwd)
	return protocol.PathSpec{Path: path, Line: line, Col: col}, nil
}

func expand(p, home string) string {
	if home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func abs(p, cwd string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if cwd == "" {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	return filepath.Clean(filepath.Join(cwd, p))
}

// splitLineCol extracts trailing `:L` or `:L:C` from raw. When neither is
// numeric the original raw is returned unchanged.
func splitLineCol(raw string) (path string, line, col int) {
	parts := strings.Split(raw, ":")
	if len(parts) == 1 {
		return raw, 0, 0
	}
	// Try `:L:C`
	if len(parts) >= 3 {
		if l, errL := strconv.Atoi(parts[len(parts)-2]); errL == nil {
			if c, errC := strconv.Atoi(parts[len(parts)-1]); errC == nil {
				return strings.Join(parts[:len(parts)-2], ":"), l, c
			}
		}
	}
	// Try trailing `:L`
	if l, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
		return strings.Join(parts[:len(parts)-1], ":"), l, 0
	}
	return raw, 0, 0
}
