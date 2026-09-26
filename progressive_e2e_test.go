package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/artschekoff/sub-translator/internal/media"
	"github.com/artschekoff/sub-translator/internal/srt"
	"github.com/artschekoff/sub-translator/internal/translate"
	"github.com/artschekoff/sub-translator/internal/whisper"
)

// writeStub drops an executable shell script on disk.
func writeStub(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// stubTranslator points every client the run builds at an httptest server that
// prefixes each text with "ES:", so translation never reaches the network.
// record, when non-nil, is called with each request's query before it is
// answered.
func stubTranslator(t *testing.T, record func(q url.Values)) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if record != nil {
			record(q)
		}
		parts := strings.Split(q.Get("q"), "|||||")
		for i := range parts {
			parts[i] = "ES:" + strings.TrimSpace(parts[i])
		}
		fmt.Fprintf(w, `[[[%q,"",null,null,3]],null,"en"]`, strings.Join(parts, " ||||| "))
	}))
	t.Cleanup(srv.Close)

	real := newTranslateClient
	newTranslateClient = func(from, to string) *translate.Client {
		return translate.NewWithBaseURL(from, to, srv.URL)
	}
	t.Cleanup(func() { newTranslateClient = real })
}

// dialogueSRT is the whisper stub body for a chunk that contains speech: two
// blocks timed as though the chunk began at zero, which is what makes the shift
// assertions meaningful.
const dialogueSRT = `printf '1\n00:00:01,000 --> 00:00:03,000\n%s one\n\n2\n00:00:05,000 --> 00:00:07,000\n%s two\n\n' "$tag" "$tag" > "$outbase.srt"`

// silentSRT is the whisper stub body for a chunk with no speech in it. whisper
// still writes a transcript; it is simply empty.
const silentSRT = `: > "$outbase.srt"`

// progressiveStubs plants every external binary a progressive run shells out to
// on PATH — ffmpeg for the extract and the slices, ffprobe for the duration,
// osascript for the completion notification, and whisper itself — and returns
// the run's working directories plus the file the whisper stub logs its -l to.
//
// whisperBody is the case-specific half of the whisper stub. It runs with
// $outbase and $tag (the chunk's basename) already set, and must write
// "$outbase.srt".
func progressiveStubs(t *testing.T, whisperBody string) (dir, binDir, tmpDir, langLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stubs are POSIX shell scripts")
	}

	dir = t.TempDir()
	binDir = filepath.Join(dir, "bin")
	tmpDir = filepath.Join(dir, "scratch")
	for _, d := range []string{binDir, tmpDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// ffmpeg is called once to extract the full WAV and once per chunk to slice
	// it. Both write their output as the last argument, and nothing downstream
	// reads the bytes, so a RIFF header is enough.
	writeStub(t, filepath.Join(binDir, "ffmpeg"), `#!/bin/sh
out=""
for a in "$@"; do out="$a"; done
printf 'RIFFWAVE' > "$out"
exit 0
`)
	// Thirty minutes, so a ten-minute chunk gives exactly three chunks.
	writeStub(t, filepath.Join(binDir, "ffprobe"), `#!/bin/sh
echo 1800.000000
exit 0
`)
	// The completion notification must not reach the real osascript from a test.
	writeStub(t, filepath.Join(binDir, "osascript"), "#!/bin/sh\nexit 0\n")

	langLog = filepath.Join(dir, "whisper-languages.txt")
	// $OUTBASE is read out of the argument list exactly as whisper-cli reads
	// -of, and -l is recorded so a swapped from/to cannot pass unnoticed.
	writeStub(t, filepath.Join(binDir, "stub-whisper"), `#!/bin/sh
outbase=""
lang=""
while [ $# -gt 0 ]; do
  case "$1" in
    -of) outbase="$2" ;;
    -l)  lang="$2" ;;
  esac
  shift
done
echo "$lang" >> "$LANG_LOG"
echo "whisper_print_progress_callback: progress = 100%" >&2
tag=$(basename "$outbase")
`+whisperBody+`
printf '{"result":{"language":"en"}}' > "$outbase.json"
exit 0
`)

	t.Setenv("LANG_LOG", langLog)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir, binDir, tmpDir, langLog
}

// TestRunProgressiveEndToEnd is the run the spec asks for: three chunks against
// a stub whisper binary, asserting the output file exists and grows after each,
// that timestamps in later chunks are offset correctly, and that the final file
// covers the whole input.
//
// It is also the only thing pinning the call-site wiring. runProgressive takes
// adjacent string parameters including from and to, and resolveChunkLanguage
// takes three strings in a row: any permutation compiles silently, and the
// failure mode of swapping from and to is a confidently transcribed,
// confidently translated, entirely wrong-language file. So the stubs record
// what whisper was told to hear and what the translation endpoint was asked
// for, and both are asserted.
func TestRunProgressiveEndToEnd(t *testing.T) {
	dir, binDir, tmpDir, langLog := progressiveStubs(t, dialogueSRT)

	input := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(input, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := media.DefaultSRTPath(input, "es")
	transcriptPath := media.DefaultSRTPath(input, "en")

	// Translation must not reach the network. The recorder also captures how big
	// the output file was at each call, which is how "grows after each chunk" is
	// observed from outside: chunk n's request happens after chunk n-1's write.
	var mu sync.Mutex
	var sizes []int64
	var sls, tls []string
	stubTranslator(t, func(q url.Values) {
		mu.Lock()
		defer mu.Unlock()
		sls = append(sls, q.Get("sl"))
		tls = append(tls, q.Get("tl"))
		var size int64
		if fi, err := os.Stat(outPath); err == nil {
			size = fi.Size()
		}
		sizes = append(sizes, size)
	})

	streams := []media.Stream{{Index: 0, CodecType: "video"}, {Index: 1, CodecType: "audio"}}
	streams[1].Tags.Language = "eng"

	opts := whisper.Options{
		Bin:   filepath.Join(binDir, "stub-whisper"),
		Model: filepath.Join(dir, "ggml-test.bin"),
	}

	if err := runProgressive(input, tmpDir, streams, -1, "en", "es",
		10*time.Minute, opts, ""); err != nil {
		t.Fatalf("runProgressive() = %v, want success", err)
	}

	// The wiring, first: whisper was told to hear the source language and the
	// endpoint was asked to translate from it into the target.
	langs, err := os.ReadFile(langLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(langs)); !slices.Equal(got, []string{"en", "en", "en"}) {
		t.Errorf("whisper was given -l %v, want [en en en] — the source language, once per chunk", got)
	}
	if !slices.Equal(sls, []string{"en", "en", "en"}) || !slices.Equal(tls, []string{"es", "es", "es"}) {
		t.Errorf("translated sl=%v tl=%v, want sl all en and tl all es", sls, tls)
	}

	// The file grew after every chunk. The first request sees no file at all,
	// because nothing is written until a chunk has been translated.
	if len(sizes) != 3 {
		t.Fatalf("translation endpoint saw %d calls, want 3 (one per chunk)", len(sizes))
	}
	if sizes[0] != 0 {
		t.Errorf("output file existed before the first chunk was translated (%d bytes)", sizes[0])
	}
	if !(sizes[1] > sizes[0] && sizes[2] > sizes[1]) {
		t.Errorf("output file sizes across chunks were %v, want strictly growing", sizes)
	}

	blocks, err := srt.Parse(outPath)
	if err != nil {
		t.Fatalf("parse %s: %v", outPath, err)
	}
	if len(blocks) != 6 {
		t.Fatalf("final file has %d blocks, want 6 (two per chunk)", len(blocks))
	}

	// The whole input is covered, and each chunk's timings were offset by that
	// chunk's start rather than restarting at zero.
	wantTimings := []string{
		"00:00:01,000 --> 00:00:03,000",
		"00:00:05,000 --> 00:00:07,000",
		"00:10:01,000 --> 00:10:03,000",
		"00:10:05,000 --> 00:10:07,000",
		"00:20:01,000 --> 00:20:03,000",
		"00:20:05,000 --> 00:20:07,000",
	}
	for i, b := range blocks {
		if b.Timing != wantTimings[i] {
			t.Errorf("block %d timing = %q, want %q", i+1, b.Timing, wantTimings[i])
		}
		// Renumbered as one ascending sequence; chunks each numbered from 1.
		if want := fmt.Sprint(i + 1); b.Index != want {
			t.Errorf("block %d index = %q, want %q", i+1, b.Index, want)
		}
		if !strings.HasPrefix(b.Text, "ES:") {
			t.Errorf("block %d text = %q, want the translated text", i+1, b.Text)
		}
	}
	// Each chunk contributed its own transcript, not the first one three times.
	if !strings.Contains(blocks[2].Text, "chunk-001") || !strings.Contains(blocks[4].Text, "chunk-002") {
		t.Errorf("chunk texts did not survive the merge: %q, %q", blocks[2].Text, blocks[4].Text)
	}

	// The source-language sidecar sits beside it, untranslated.
	srcBlocks, err := srt.Parse(transcriptPath)
	if err != nil {
		t.Fatalf("parse %s: %v", transcriptPath, err)
	}
	if len(srcBlocks) != 6 {
		t.Errorf("transcript has %d blocks, want 6", len(srcBlocks))
	}
	if strings.HasPrefix(srcBlocks[0].Text, "ES:") {
		t.Errorf("transcript block 1 = %q, want the untranslated text", srcBlocks[0].Text)
	}

	// Chunk WAVs are removed as whisper finishes with them, not at the end of
	// the run: keeping all of them roughly doubles peak scratch usage.
	leftovers, err := filepath.Glob(filepath.Join(tmpDir, "chunk-*.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Errorf("chunk WAVs survived the run: %v", leftovers)
	}
}

// A silent opening must not cost the user the whole film. Ten minutes with no
// speech in them is ordinary — an overture, a title sequence, a commentary track
// that starts quiet — and before this branch the fast path transcribed such a
// film without complaint. Guarding per chunk instead of per run would refuse it,
// turning a full subtitle file into no file at all: a regression, not a fix.
// Whether a film has any speech in it is a question only the whole run can
// answer.
func TestRunProgressiveToleratesASilentOpeningChunk(t *testing.T) {
	dir, binDir, tmpDir, _ := progressiveStubs(t, `case "$tag" in
  chunk-000) `+silentSRT+` ;;
  *) `+dialogueSRT+` ;;
esac`)

	input := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(input, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := media.DefaultSRTPath(input, "es")
	stubTranslator(t, nil)

	streams := []media.Stream{{Index: 0, CodecType: "video"}, {Index: 1, CodecType: "audio"}}
	streams[1].Tags.Language = "eng"
	opts := whisper.Options{Bin: filepath.Join(binDir, "stub-whisper"), Model: filepath.Join(dir, "ggml-test.bin")}

	if err := runProgressive(input, tmpDir, streams, -1, "en", "es",
		10*time.Minute, opts, ""); err != nil {
		t.Fatalf("runProgressive() = %v, want success — a silent first chunk is not a silent film", err)
	}

	blocks, err := srt.Parse(outPath)
	if err != nil {
		t.Fatalf("parse %s: %v", outPath, err)
	}
	if len(blocks) != 4 {
		t.Fatalf("final file has %d blocks, want 4 (two each from chunks 2 and 3)", len(blocks))
	}
	// The silent chunk contributes nothing, and the chunks that follow keep the
	// offsets belonging to their own position in the film.
	wantTimings := []string{
		"00:10:01,000 --> 00:10:03,000",
		"00:10:05,000 --> 00:10:07,000",
		"00:20:01,000 --> 00:20:03,000",
		"00:20:05,000 --> 00:20:07,000",
	}
	for i, b := range blocks {
		if b.Timing != wantTimings[i] {
			t.Errorf("block %d timing = %q, want %q", i+1, b.Timing, wantTimings[i])
		}
		if want := fmt.Sprint(i + 1); b.Index != want {
			t.Errorf("block %d index = %q, want %q", i+1, b.Index, want)
		}
	}
	if !strings.Contains(blocks[0].Text, "chunk-001") || !strings.Contains(blocks[2].Text, "chunk-002") {
		t.Errorf("wrong chunks survived the merge: %q, %q", blocks[0].Text, blocks[2].Text)
	}
}

// A film whose audio carries no speech anywhere is the case the guard exists
// for, and it fails with the same message and the same meaning as the non-fast
// path rather than announcing that an empty file covers the whole film.
func TestRunProgressiveFailsWhenEveryChunkIsSilent(t *testing.T) {
	dir, binDir, tmpDir, _ := progressiveStubs(t, silentSRT)

	input := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(input, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := media.DefaultSRTPath(input, "es")
	stubTranslator(t, nil)

	streams := []media.Stream{{Index: 0, CodecType: "video"}, {Index: 1, CodecType: "audio"}}
	streams[1].Tags.Language = "eng"
	opts := whisper.Options{Bin: filepath.Join(binDir, "stub-whisper"), Model: filepath.Join(dir, "ggml-test.bin")}

	err := runProgressive(input, tmpDir, streams, -1, "en", "es",
		10*time.Minute, opts, "")
	if err == nil {
		t.Fatal("runProgressive() = nil, want an error — nothing was transcribed")
	}
	if !errors.Is(err, errEmptyTranscript) {
		t.Errorf("error = %v, want the empty-transcript message the non-fast path gives", err)
	}
	// There is no coverage to report, so the message must not invent any.
	if strings.Contains(err.Error(), "already covers") {
		t.Errorf("error = %v, want no coverage claim: nothing was ever written", err)
	}
	if _, err := os.Stat(outPath); err == nil {
		t.Errorf("%s was written although there was nothing to put in it", outPath)
	}
}
