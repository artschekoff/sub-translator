package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
// answers like the chat endpoint, prefixing each line with "ES:", so translation
// never reaches the network. record, when non-nil, is called with each request's
// system prompt — which is where the languages are named — before it is answered.
func stubTranslator(t *testing.T, record func(system string)) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("the translation request is not JSON: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var system, user string
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				system = m.Content
			case "user":
				user = m.Content
			}
		}
		if record != nil {
			record(system)
		}
		var payload struct {
			Translate map[string]string `json:"translate"`
		}
		if err := json.Unmarshal([]byte(user), &payload); err != nil {
			t.Errorf("the user message is not the batch payload: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		out := make(map[string]string, len(payload.Translate))
		for k, v := range payload.Translate {
			out[k] = "ES:" + v
		}
		content, _ := json.Marshal(out)
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, content)
	}))
	t.Cleanup(srv.Close)

	real := newTranslateClient
	newTranslateClient = func(o translate.Options) (translate.Translator, error) {
		o.BaseURL = srv.URL
		if o.APIKey == "" {
			// The key is mandatory now, and these tests are about the run rather
			// than about where the key came from.
			o.APIKey = "sk-test"
		}
		return translate.NewTranslator(o)
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
	// A run's ending is package state; a test binary hosts many runs.
	freshEnding(t)
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

// subtitleSourceStubs plants the ffprobe and ffmpeg stubs a compiled-binary run
// under -source sub needs, in dir/bin, and returns that directory.
//
// It is a sibling of progressiveStubs, not a replacement for it in this role:
// progressiveStubs' ffprobe answers every call with a bare duration number,
// which is what runProgressive's direct ContainerDuration calls want but is not
// valid JSON — and every compiled-binary run reaches media.Probe (-show_streams,
// JSON) before it reaches anything else, on every -source. Changing
// progressiveStubs to speak JSON would ripple into the several tests above that
// already depend on its current shape, so this is a separate stub set instead:
// one subtitle stream (no confirmation prompt, no whisper needed at all), and an
// ffmpeg extraction stub that writes srtBody — a real, parseable SRT body — to
// whatever output path it is given, standing in for the real subtitle track
// media.ExtractSubtitle would otherwise pull out of the container.
func subtitleSourceStubs(t *testing.T, dir, srtBody string) (binDir string) {
	t.Helper()
	// A run's ending is package state; a test binary hosts many runs.
	freshEnding(t)
	if runtime.GOOS == "windows" {
		t.Skip("the stubs are POSIX shell scripts")
	}

	binDir = filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeStub(t, filepath.Join(binDir, "ffprobe"), "#!/bin/sh\n"+`cat <<JSON
{"streams":[
 {"index":0,"codec_type":"subtitle","codec_name":"srt","tags":{"language":"eng"}}
],"format":{"duration":"120.5"}}
JSON
`)
	// ExtractSubtitle's last argument is always the output path, exactly like the
	// audio-extraction stub above; here the bytes matter, so a real SRT body goes
	// in rather than a placeholder.
	writeStub(t, filepath.Join(binDir, "ffmpeg"), `#!/bin/sh
out=""
for a in "$@"; do out="$a"; done
cat > "$out" <<'SRTBODY'
`+srtBody+`
SRTBODY
exit 0
`)

	return binDir
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
	var prompts []string
	stubTranslator(t, func(system string) {
		mu.Lock()
		defer mu.Unlock()
		prompts = append(prompts, system)
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
		10*time.Minute, opts, translate.Options{}, ""); err != nil {
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
	if len(prompts) != 3 {
		t.Fatalf("%d translation requests, want one per chunk", len(prompts))
	}
	for i, p := range prompts {
		// A swapped from/to would read "from es into en" here, which is the whole
		// point of asserting on the instruction rather than on the result.
		if !strings.Contains(p, "from en into es") {
			t.Errorf("chunk %d was asked to translate the wrong way round:\n%s", i+1, p)
		}
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
		10*time.Minute, opts, translate.Options{}, ""); err != nil {
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
		10*time.Minute, opts, translate.Options{}, "")
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

// captureStdout runs fn with os.Stdout redirected and returns what was written.
// The reader drains in a goroutine because a progressive run prints far more than
// a pipe buffer holds.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	real := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		io.Copy(&b, r)
		done <- b.String()
	}()

	fn()

	os.Stdout = real
	w.Close()
	out := <-done
	r.Close()
	return out
}

// -fast is what people reach for on long films, which are the expensive ones, so
// it must say which provider and model it is about to spend on. Nothing else in
// that loop names it.
func TestRunProgressiveNamesTheProvider(t *testing.T) {
	dir, binDir, tmpDir, _ := progressiveStubs(t, silentSRT)

	input := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(input, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubTranslator(t, nil)

	streams := []media.Stream{{Index: 0, CodecType: "video"}, {Index: 1, CodecType: "audio"}}
	streams[1].Tags.Language = "eng"
	opts := whisper.Options{Bin: filepath.Join(binDir, "stub-whisper"), Model: filepath.Join(dir, "ggml-test.bin")}
	topts := translate.Options{
		APIKey:  "sk-test",
		Model:   translate.DefaultModel,
		Workers: 4,
	}

	out := captureStdout(t, func() {
		// A silent stub means this run ends in the empty-transcript error, which is
		// beside the point: the line is printed before the first chunk either way.
		_ = runProgressive(input, tmpDir, streams, -1, "en", "es",
			10*time.Minute, opts, topts, "")
	})

	for _, want := range []string{"openai", translate.DefaultModel, "4 workers"} {
		if !strings.Contains(out, want) {
			t.Errorf("a -fast run never named %q:\n%s", want, out)
		}
	}
}
