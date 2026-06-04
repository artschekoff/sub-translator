package srt

import (
	"fmt"
	"os"
	"strings"
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

func Write(path string, blocks []Block) error {
	var sb strings.Builder
	for i, b := range blocks {
		sb.WriteString(b.Index)
		sb.WriteByte('\n')
		sb.WriteString(b.Timing)
		sb.WriteByte('\n')
		sb.WriteString(b.Text)
		sb.WriteString("\n\n")
		_ = i
	}
	return os.WriteFile(path, []byte(strings.TrimRight(sb.String(), "\n")+"\n"), 0644)
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
