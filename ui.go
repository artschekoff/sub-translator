package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/artschekoff/sub-translator/internal/media"
)

// stdin is shared by every prompt. A fresh bufio.Reader per prompt would read
// ahead into its own buffer and swallow the next answer when input is piped.
var stdin = bufio.NewReader(os.Stdin)

// formatTracks renders the subtitle tracks found in the input. Tracks with no
// language or title tag are common in the wild, so they are marked rather than
// rendered as blank columns.
func formatTracks(subs []media.Stream) string {
	var b strings.Builder
	b.WriteString("Subtitle tracks:\n")
	for _, s := range subs {
		fmt.Fprintf(&b, "  #%-3d lang=%-8s %s\n",
			s.Index, orNone(s.Tags.Language), orNone(s.Tags.Title))
	}
	return b.String()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// readLang prompts for a language code and reads one from r, re-asking on blank
// input. It returns an error once r is exhausted, which is what happens when
// stdin is a pipe or /dev/null rather than a terminal.
func readLang(r *bufio.Reader, w io.Writer, label string) (string, error) {
	for {
		fmt.Fprintf(w, "%s language code (e.g. en, ru, es, fr): ", label)
		line, err := r.ReadString('\n')
		if lang := strings.TrimSpace(line); lang != "" {
			return lang, nil
		}
		if err != nil {
			fmt.Fprintln(w)
			return "", fmt.Errorf("no %s language given", strings.ToLower(label))
		}
	}
}

// promptLang is the interactive wrapper used by main: it reads from the shared
// stdin, prompts on stderr so piped stdout stays clean, and exits on failure.
func promptLang(label string) string {
	lang, err := readLang(stdin, os.Stderr, label)
	if err != nil {
		fatalf("%v", err)
	}
	return lang
}
