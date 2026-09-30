package probe

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// maxLineLen truncates pathological single-line files (minified JS, a log with
// no newlines) so one line cannot consume the whole context window.
const maxLineLen = 400

// HumanSize renders a byte count compactly, as the probes do.
func HumanSize(n int64) string { return humanSize(n) }

// humanSize renders a byte count compactly. -1 means unknown.
func humanSize(n int64) string {
	if n < 0 {
		return "size?"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	val := float64(n)
	for _, suffix := range []string{"K", "M", "G", "T"} {
		val /= unit
		if val < unit {
			if val < 10 {
				return fmt.Sprintf("%.1f%s", val, suffix)
			}
			return fmt.Sprintf("%.0f%s", val, suffix)
		}
	}
	return fmt.Sprintf("%.0fP", val/unit)
}

// clipLine trims a line to maxLineLen, marking the cut.
func clipLine(s string) string {
	s = strings.TrimRight(s, "\r")
	if len(s) <= maxLineLen {
		return s
	}
	return s[:maxLineLen] + "…(clipped)"
}

// firstLine returns the first non-empty line of s, clipped.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return clipLine(t)
		}
	}
	return ""
}

// readHeadLines reads up to n lines from the current offset, capped at
// DefaultMaxFileRead bytes total.
func readHeadLines(ctx context.Context, r io.Reader, n int) ([]string, error) {
	sc := bufio.NewScanner(io.LimitReader(r, DefaultMaxFileRead))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	var lines []string
	for len(lines) < n && sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lines = append(lines, clipLine(sc.Text()))
	}
	if err := sc.Err(); err != nil && err != bufio.ErrTooLong {
		return nil, err
	}
	return lines, nil
}

// readTailLines returns the last n lines of a file, reading at most
// DefaultMaxFileRead bytes from the end rather than scanning the whole file.
func readTailLines(f *os.File, size int64, n int) ([]string, error) {
	readFrom := int64(0)
	chunk := size
	if size > DefaultMaxFileRead {
		readFrom = size - DefaultMaxFileRead
		chunk = DefaultMaxFileRead
	}
	buf := make([]byte, chunk)
	if _, err := f.ReadAt(buf, readFrom); err != nil && err != io.EOF {
		return nil, err
	}

	raw := strings.Split(string(bytes.TrimRight(buf, "\n")), "\n")
	// A partial first line is an artifact of chunked reading, not real data.
	if readFrom > 0 && len(raw) > 1 {
		raw = raw[1:]
	}
	if len(raw) > n {
		raw = raw[len(raw)-n:]
	}
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		out = append(out, clipLine(l))
	}
	return out, nil
}

// countLines counts newlines and reports whether the content looks binary.
// It reads the whole file but holds only a fixed buffer.
func countLines(ctx context.Context, r io.Reader) (lines int, binary bool, err error) {
	buf := make([]byte, 64<<10)
	checked := false
	for {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		n, readErr := r.Read(buf)
		if n > 0 {
			if !checked {
				if bytes.IndexByte(buf[:min(n, sniffBytes)], 0) >= 0 {
					return 0, true, nil
				}
				checked = true
			}
			lines += bytes.Count(buf[:n], []byte{'\n'})
		}
		if readErr == io.EOF {
			return lines, false, nil
		}
		if readErr != nil {
			return lines, false, readErr
		}
	}
}

type grepHit struct {
	no   int
	text string
}

// grepFile scans one file for re, returning at most limit hits. It stops at
// DefaultMaxFileRead bytes and skips files that sniff as binary.
func grepFile(ctx context.Context, path string, re *regexp.Regexp, limit int) (bool, []grepHit) {
	if limit <= 0 {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, nil
	}
	defer f.Close()

	sniff := make([]byte, sniffBytes)
	n, _ := io.ReadFull(f, sniff)
	if bytes.IndexByte(sniff[:n], 0) >= 0 {
		return false, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, nil
	}

	sc := bufio.NewScanner(io.LimitReader(f, DefaultMaxFileRead))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	var hits []grepHit
	for lineNo := 1; sc.Scan(); lineNo++ {
		if ctx.Err() != nil {
			break
		}
		if re.MatchString(sc.Text()) {
			hits = append(hits, grepHit{no: lineNo, text: clipLine(sc.Text())})
			if len(hits) >= limit {
				break
			}
		}
	}
	return len(hits) > 0, hits
}
