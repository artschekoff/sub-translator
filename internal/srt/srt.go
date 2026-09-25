package srt

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Block struct {
	Index  string
	Timing string
	Text   string
}

func Parse(path string) ([]Block, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	parts := strings.Split(strings.TrimSpace(content), "\n\n")
	var blocks []Block
	for _, part := range parts {
		lines := strings.SplitN(strings.TrimSpace(part), "\n", 3)
		if len(lines) < 3 {
			continue
		}
		blocks = append(blocks, Block{
			Index:  lines[0],
			Timing: lines[1],
			Text:   lines[2],
		})
	}
	return blocks, nil
}

// Write saves blocks to path. A viewer using -fast may have the file open in
// their player while this runs, so it never truncates the target in place:
// the content is staged in a temp file in the same directory and moved into
// place with os.Rename, which POSIX guarantees is atomic. A reader only ever
// sees the previous complete file or the next one, never a partial write, and
// a failure partway through (a full disk, a NAS hiccup) leaves the existing
// file untouched instead of destroying it.
func Write(path string, blocks []Block) error {
	var sb strings.Builder
	for _, b := range blocks {
		sb.WriteString(b.Index)
		sb.WriteByte('\n')
		sb.WriteString(b.Timing)
		sb.WriteByte('\n')
		sb.WriteString(b.Text)
		sb.WriteString("\n\n")
	}
	data := []byte(strings.TrimRight(sb.String(), "\n") + "\n")

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".srt-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Whatever happens below, a stray temp file must not survive this call: a
	// leftover .tmp next to the target would defeat the point of the rename if
	// Write is retried or the directory is inspected. Remove is a no-op once
	// Rename has already moved the file away.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func Texts(blocks []Block) []string {
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i] = b.Text
	}
	return out
}

func WithTexts(blocks []Block, texts []string) ([]Block, error) {
	if len(texts) != len(blocks) {
		return nil, fmt.Errorf("text count mismatch: got %d, want %d", len(texts), len(blocks))
	}
	result := make([]Block, len(blocks))
	for i, b := range blocks {
		result[i] = Block{Index: b.Index, Timing: b.Timing, Text: texts[i]}
	}
	return result, nil
}

// parseTimestamp reads one side of an SRT timing line: "00:01:23,456".
func parseTimestamp(s string) (time.Duration, error) {
	var h, m, sec, ms int
	n, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d:%d,%d", &h, &m, &sec, &ms)
	if err != nil || n != 4 {
		return 0, fmt.Errorf("malformed SRT timestamp %q", s)
	}
	return time.Duration(h)*time.Hour +
		time.Duration(m)*time.Minute +
		time.Duration(sec)*time.Second +
		time.Duration(ms)*time.Millisecond, nil
}

func formatTimestamp(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	d -= s * time.Second
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, d/time.Millisecond)
}

// Shift moves every block by the given offset. A chunk of a longer film is
// transcribed as though it began at zero, so merging it back requires adding the
// chunk's start time to both ends of every timing.
func Shift(blocks []Block, by time.Duration) ([]Block, error) {
	out := make([]Block, len(blocks))
	for i, b := range blocks {
		left, right, ok := strings.Cut(b.Timing, "-->")
		if !ok {
			return nil, fmt.Errorf("block %d: malformed timing %q", i+1, b.Timing)
		}
		start, err := parseTimestamp(left)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", i+1, err)
		}
		end, err := parseTimestamp(right)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", i+1, err)
		}
		out[i] = Block{
			Index:  b.Index,
			Timing: formatTimestamp(start+by) + " --> " + formatTimestamp(end+by),
			Text:   b.Text,
		}
	}
	return out, nil
}

// Renumber rewrites the index lines as one ascending sequence. Chunks each start
// their own numbering at 1, and Write emits the index verbatim, so a merged file
// is invalid until this has run.
func Renumber(blocks []Block) []Block {
	out := make([]Block, len(blocks))
	for i, b := range blocks {
		out[i] = Block{Index: strconv.Itoa(i + 1), Timing: b.Timing, Text: b.Text}
	}
	return out
}
