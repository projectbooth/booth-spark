package runs

import (
	"bytes"
	"errors"
	"io"
	"unicode/utf8"
)

// The log cursor (GET /v1/applications/{id}/logs?from=<offset>, ADR 0119 amendment 3): offsets are
// byte offsets into the driver container's whole log, the same while the run lives and after it
// ends. Every byte keeps its offset: a byte that isn't valid UTF-8 is shown as '?' (one for one,
// so the stored text is valid for PostgreSQL), a chunk never ends inside a character, and a full
// chunk ends at a line's end when it has one.

// LogChunkBytes is the most one cursor read returns.
const LogChunkBytes = 1 << 20

// sanitize replaces each byte that isn't part of valid UTF-8 with '?', keeping the length.
func sanitize(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n <= 1 {
			out = append(out, '?')
			b = b[1:]
			continue
		}
		out = append(out, b[:n]...)
		b = b[n:]
	}
	return out
}

// cutChunk trims a chunk read from a live log: a chunk that filled max ends after its last newline
// (if any), and no chunk ends inside a multi-byte character (the rest comes with the next read).
func cutChunk(b []byte, max int) []byte {
	if len(b) >= max {
		b = b[:max]
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			return b[:i+1]
		}
	}
	// Drop an incomplete character at the end (at most 3 bytes).
	for k := 1; k <= 3 && k <= len(b); k++ {
		c := b[len(b)-k]
		if c < 0x80 {
			break
		}
		if utf8.RuneStart(c) {
			if !utf8.FullRune(b[len(b)-k:]) {
				return b[:len(b)-k]
			}
			break
		}
	}
	return b
}

// readFrom skips from bytes of r and returns up to max bytes after them, cut by cutChunk, and how
// far r reached (less than from when the log is shorter).
func readFrom(r io.Reader, from int64, max int) ([]byte, int64, error) {
	skipped, err := io.CopyN(io.Discard, r, from)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, skipped, err
	}
	if skipped < from {
		return nil, skipped, nil
	}
	buf, err := io.ReadAll(io.LimitReader(r, int64(max)))
	if err != nil {
		return nil, from, err
	}
	return sanitize(cutChunk(buf, max)), from, nil
}

// keepTail reads all of r and keeps its last max bytes, starting at a line's start when it had to
// drop anything, and the offset of the kept text in r.
func keepTail(r io.Reader, max int64) ([]byte, int64, error) {
	var total int64
	var keep []byte
	buf := make([]byte, 64<<10)
	for {
		n, err := r.Read(buf)
		total += int64(n)
		keep = append(keep, buf[:n]...)
		if int64(len(keep)) > 2*max {
			keep = append([]byte(nil), keep[int64(len(keep))-max:]...)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
	}
	if int64(len(keep)) > max {
		keep = keep[int64(len(keep))-max:]
	}
	offset := total - int64(len(keep))
	if offset > 0 {
		if i := bytes.IndexByte(keep, '\n'); i >= 0 && i+1 < len(keep) {
			keep, offset = keep[i+1:], offset+int64(i+1)
		}
	}
	return sanitize(keep), offset, nil
}
