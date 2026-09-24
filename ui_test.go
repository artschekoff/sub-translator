package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/artschekoff/sub-translator/internal/media"
)

func sub(index int, lang, title string) media.Stream {
	var s media.Stream
	s.Index = index
	s.CodecType = "subtitle"
	s.CodecName = "subrip"
	s.Tags.Language = lang
	s.Tags.Title = title
	return s
}

func TestFormatTracksListsEveryTrack(t *testing.T) {
	got := formatTracks([]media.Stream{
		sub(2, "rus", "Rus, SRT"),
		sub(3, "eng", "Eng, SRT"),
	})

	for _, want := range []string{"#2", "rus", "Rus, SRT", "#3", "eng", "Eng, SRT"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatTracks output missing %q:\n%s", want, got)
		}
	}
}

// Files like Korabl.Prizrak.1.mkv carry subtitle tracks with no language and no
// title at all. Those must still be listed, and must not render as blank space.
func TestFormatTracksMarksMissingTags(t *testing.T) {
	got := formatTracks([]media.Stream{sub(2, "", "")})

	if !strings.Contains(got, "#2") {
		t.Errorf("untagged track not listed:\n%s", got)
	}
	if strings.Count(got, "(none)") != 2 {
		t.Errorf("want both language and title marked (none), got:\n%s", got)
	}
}

func TestReadLangTrimsAndReturns(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("  ru \n"))
	var out bytes.Buffer

	got, err := readLang(r, &out, "Source")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ru" {
		t.Errorf("readLang = %q, want %q", got, "ru")
	}
	if !strings.Contains(out.String(), "Source") {
		t.Errorf("prompt should name which language is being asked for, got %q", out.String())
	}
}

func TestReadLangSkipsBlankLines(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\n\nfr\n"))
	var out bytes.Buffer

	got, err := readLang(r, &out, "Target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "fr" {
		t.Errorf("readLang = %q, want %q", got, "fr")
	}
}

func TestReadLangErrorsOnEOF(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(""))
	var out bytes.Buffer

	if _, err := readLang(r, &out, "Source"); err == nil {
		t.Fatal("want error when stdin is not interactive, got nil")
	}
}

// Both languages are read from one shared reader. A per-call bufio.Reader would
// swallow the second line into the first reader's buffer, so piping two answers
// must work.
func TestReadLangTwiceFromOneReader(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("ru\nen\n"))
	var out bytes.Buffer

	first, err := readLang(r, &out, "Source")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	second, err := readLang(r, &out, "Target")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if first != "ru" || second != "en" {
		t.Errorf("got (%q, %q), want (ru, en)", first, second)
	}
}

func TestReadConfirmAcceptsYesForms(t *testing.T) {
	for _, in := range []string{"y\n", "Y\n", "yes\n", "  yes  \n"} {
		var out strings.Builder
		got, err := readConfirm(bufio.NewReader(strings.NewReader(in)), &out, "Transcribe?")
		if err != nil {
			t.Errorf("readConfirm(%q): %v", in, err)
			continue
		}
		if !got {
			t.Errorf("readConfirm(%q) = false, want true", in)
		}
	}
}

// Anything that is not an explicit yes means no. Transcription is expensive and
// must not start on a stray keypress.
func TestReadConfirmTreatsEverythingElseAsNo(t *testing.T) {
	for _, in := range []string{"n\n", "no\n", "\n", "maybe\n"} {
		var out strings.Builder
		got, err := readConfirm(bufio.NewReader(strings.NewReader(in)), &out, "Transcribe?")
		if err != nil {
			t.Errorf("readConfirm(%q): %v", in, err)
			continue
		}
		if got {
			t.Errorf("readConfirm(%q) = true, want false", in)
		}
	}
}

func TestReadConfirmShowsTheQuestion(t *testing.T) {
	var out strings.Builder
	if _, err := readConfirm(bufio.NewReader(strings.NewReader("y\n")), &out, "Transcribe?"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Transcribe?") {
		t.Errorf("prompt %q does not contain the question", out.String())
	}
}

func TestReadConfirmErrorsOnExhaustedInput(t *testing.T) {
	var out strings.Builder
	if _, err := readConfirm(bufio.NewReader(strings.NewReader("")), &out, "Transcribe?"); err == nil {
		t.Error("want an error when there is no answer to read")
	}
}
