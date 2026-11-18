package files

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Path limits.
const (
	MaxPathLen     = 1024
	MaxSegmentLen  = 255
	MaxPathDepth   = 32
	maxDisplayName = 80
)

// ErrUnsafePath is wrapped by CleanPath errors.
var ErrUnsafePath = errors.New("unsafe path")

// windowsReserved are device names that cannot be used as file names on
// Windows, with or without an extension.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// CleanPath validates a user- or archive-supplied relative path and returns
// it in canonical slash-separated form ("src/main.go"). Backslashes are
// treated as separators and "." segments are dropped. It rejects anything
// that could escape the project root or be misinterpreted by a filesystem:
// absolute paths, drive letters, ".." segments, control characters, invalid
// UTF-8, Windows reserved names and characters, and overly long or deep paths.
//
// The cleaned path is only ever used as metadata; stored content lives under
// server-generated names, so a path can never select a location on disk.
func CleanPath(p string) (string, error) {
	fail := func(reason string) (string, error) {
		return "", fmt.Errorf("%w %q: %s", ErrUnsafePath, displayName(p), reason)
	}
	if p == "" {
		return fail("empty path")
	}
	if !utf8.ValidString(p) {
		return fail("not valid UTF-8")
	}
	if len(p) > MaxPathLen {
		return fail("path too long")
	}
	if strings.ContainsFunc(p, unicode.IsControl) {
		return fail("contains control characters")
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if strings.HasPrefix(p, "/") {
		return fail("absolute path")
	}
	if len(p) >= 2 && p[1] == ':' {
		return fail("drive letter")
	}

	var out []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			return fail("parent directory reference")
		}
		if len(seg) > MaxSegmentLen {
			return fail("name too long")
		}
		if strings.ContainsAny(seg, `<>:"|?*`) {
			return fail("contains characters that are not allowed in file names")
		}
		if strings.HasSuffix(seg, " ") || strings.HasSuffix(seg, ".") {
			return fail("name ends with a space or dot")
		}
		base, _, _ := strings.Cut(seg, ".")
		if windowsReserved[strings.ToUpper(strings.TrimSpace(base))] {
			return fail("reserved device name")
		}
		out = append(out, seg)
	}
	if len(out) == 0 {
		return fail("empty path")
	}
	if len(out) > MaxPathDepth {
		return fail("too many nested directories")
	}
	return strings.Join(out, "/"), nil
}

// displayName shortens and sanitizes a raw path for use in messages.
func displayName(p string) string {
	p = strings.ToValidUTF8(p, "?")
	p = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, p)
	if utf8.RuneCountInString(p) > maxDisplayName {
		r := []rune(p)
		p = string(r[:maxDisplayName-1]) + "…"
	}
	return p
}
