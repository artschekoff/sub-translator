package media

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/artschekoff/sub-translator/internal/proc"
)

type Stream struct {
	Index     int    `json:"index"`
	CodecType string `json:"codec_type"`
	CodecName string `json:"codec_name"`
	Tags      struct {
		Language string `json:"language"`
		Title    string `json:"title"`
	} `json:"tags"`
}

type ProbeResult struct {
	Streams []Stream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func Probe(path string) ([]Stream, error) {
	out, err := exec.Command("ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		path,
	).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var result ProbeResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}
	return result.Streams, nil
}

// ContainerDuration reports how long the file runs. ffprobe gives it as a
// string of seconds in the format object, which is why this makes its own
// ffprobe call with -show_format rather than folding into Probe, which still
// asks only for -show_streams and is unchanged.
func ContainerDuration(path string) (time.Duration, error) {
	out, err := exec.Command("ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		path,
	).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe duration: %w", err)
	}
	var result ProbeResult
	if err := json.Unmarshal(out, &result); err != nil {
		return 0, fmt.Errorf("parse ffprobe output: %w", err)
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(result.Format.Duration), 64)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", result.Format.Duration, err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

func SubtitleStreams(streams []Stream) []Stream {
	var subs []Stream
	for _, s := range streams {
		if s.CodecType == "subtitle" {
			subs = append(subs, s)
		}
	}
	return subs
}

// iso6392 maps ISO 639-1 (2-letter) → ISO 639-2 (3-letter) codes used in MKV containers.
var iso6392 = map[string]string{
	"en": "eng", "es": "spa", "fr": "fra", "de": "deu", "it": "ita",
	"pt": "por", "ru": "rus", "zh": "zho", "ja": "jpn", "ko": "kor",
	"ar": "ara", "pl": "pol", "nl": "nld", "sv": "swe", "no": "nor",
	"da": "dan", "fi": "fin", "cs": "ces", "hu": "hun", "ro": "ron",
	"tr": "tur", "uk": "ukr",
}

func normLang(lang string) string {
	l := strings.ToLower(lang)
	if v, ok := iso6392[l]; ok {
		return v
	}
	return l
}

// findByLang returns the first stream of the given codec type whose language
// tag matches lang once both are normalised to ISO 639-2.
func findByLang(streams []Stream, lang, codecType string) (Stream, bool) {
	norm := normLang(lang)
	for _, s := range streams {
		if s.CodecType == codecType && normLang(s.Tags.Language) == norm {
			return s, true
		}
	}
	return Stream{}, false
}

func FindSubtitleByLang(streams []Stream, lang string) (Stream, bool) {
	return findByLang(streams, lang, "subtitle")
}

// ffmpegTailLines is how many lines of a failing ffmpeg's diagnostics travel in
// the returned error. ffmpeg says what actually went wrong at the end of its
// output; the banner ahead of it — the version, the build flags, the
// configuration — explains nothing about the failure and is what made passing
// the whole stream through so noisy.
const ffmpegTailLines = 20

// runFFmpeg runs an ffmpeg command with its stderr captured instead of
// inherited, and folds what ffmpeg said into the error when it fails.
//
// Nothing but this process may write the file descriptor the event stream uses.
// ffmpeg rewrites its progress line in place, terminated by a carriage return
// and never by a newline, so a JSON event emitted while a progress line is in
// flight lands in the middle of it and no line-oriented consumer can read
// either one — which is how a cancelled run lost the single terminal event it
// has. whisper.Run has always piped its child's stderr for this reason; this is
// the same treatment for the four ffmpeg steps.
//
// The diagnostics are not thrown away. Passing stderr through was the only
// thing that put a failing ffmpeg's explanation in front of the user, so it
// travels in the error instead, which is where the explanation of a failure
// belongs — and it now reaches an -events json consumer too, inside the error
// event's message, where inherited stderr never put it.
//
// report, when non-nil, is called with the elapsed media time parsed out of
// each progress line as it is scanned — while the diagnostics are still being
// drained, not after — so a caller watching a long extraction sees it move
// rather than jump once at the end.
func runFFmpeg(cmd *exec.Cmd, report func(time.Duration)) error {
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	// Every ffmpeg this package runs comes through here — the subtitle
	// extraction, the audio extraction, the mux and each -fast slice — so this
	// one call is what makes all four killable when a run is cancelled.
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	// Drained to EOF before Wait: StderrPipe's contract, and the pipe filling up
	// would otherwise stall ffmpeg itself.
	tail := ffmpegDiagnostics(stderr, report)
	if err := cmd.Wait(); err != nil {
		if tail != "" {
			return fmt.Errorf("ffmpeg: %w\n%s", err, tail)
		}
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
}

// ffmpegDiagnostics drains r and returns the tail of what ffmpeg had to say,
// with the progress chatter dropped from the tail — but not thrown away
// outright: report, when non-nil, gets the elapsed time parsed out of each
// progress line, which is the only trace of ffmpeg's progress left once its
// stderr stopped being passed straight through.
func ffmpegDiagnostics(r io.Reader, report func(time.Duration)) string {
	var tail []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	sc.Split(scanLinesOrCR)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if isProgressLine(line) {
			if report != nil {
				if d, ok := parseFFmpegTime(line); ok {
					report(d)
				}
			}
			continue
		}
		tail = append(tail, line)
		if len(tail) > ffmpegTailLines {
			tail = tail[1:]
		}
	}
	if sc.Err() != nil {
		// A line too long for the buffer must not leave the pipe unread: Wait
		// would then block on a child stalled writing into a full pipe.
		io.Copy(io.Discard, r)
	}
	return strings.Join(tail, "\n")
}

// scanLinesOrCR splits on a carriage return as well as a newline. Without it
// ffmpeg's entire progress stream is a single enormous line and the diagnostic
// printed after it cannot be separated from it.
func scanLinesOrCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// isProgressLine reports whether a line is one of ffmpeg's in-place progress
// updates: a run of key=value pairs opening with frame= when there is video or
// size= when there is not. They carry nothing about a failure, and a long
// extraction writes thousands of them — enough to push the real diagnostic out
// of any tail that kept them.
func isProgressLine(line string) bool {
	s := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(s, "size=") || strings.HasPrefix(s, "frame=")
}

func ExtractSubtitle(input string, streamIndex int, outSRT string, onProgress Progress) error {
	return runFFmpeg(exec.Command("ffmpeg", "-y",
		"-i", input,
		"-map", fmt.Sprintf("0:%d", streamIndex),
		outSRT,
	), progressReporter(input, onProgress))
}

func AudioStreams(streams []Stream) []Stream {
	var out []Stream
	for _, s := range streams {
		if s.CodecType == "audio" {
			out = append(out, s)
		}
	}
	return out
}

func FindAudioByLang(streams []Stream, lang string) (Stream, bool) {
	return findByLang(streams, lang, "audio")
}

// ExtractAudio decodes one audio stream to the only format whisper.cpp's
// bundled WAV reader accepts: 16 kHz mono signed 16-bit PCM. Whisper resamples
// to exactly this internally, so nothing is lost by doing it here.
func ExtractAudio(input string, streamIndex int, outWAV string, onProgress Progress) error {
	return runFFmpeg(exec.Command("ffmpeg", extractAudioArgs(input, streamIndex, outWAV)...),
		progressReporter(input, onProgress))
}

func extractAudioArgs(input string, streamIndex int, outWAV string) []string {
	return []string{
		"-y",
		"-i", input,
		"-map", fmt.Sprintf("0:%d", streamIndex),
		"-vn",
		"-ac", "1",
		"-ar", "16000",
		"-c:a", "pcm_s16le",
		outWAV,
	}
}

// ContainerSupportsEmbeddedSubs returns false for containers that can't embed subtitle tracks.
func ContainerSupportsEmbeddedSubs(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".avi":
		return false
	default:
		return true
	}
}

func MuxSubtitle(input, srtPath, lang, title, output string, onProgress Progress) error {
	streams, err := Probe(input)
	if err != nil {
		return err
	}
	subCount := 0
	for _, s := range streams {
		if s.CodecType == "subtitle" {
			subCount++
		}
	}

	// MP4/MOV containers require mov_text codec; MKV uses subrip copy.
	subCodec := "copy"
	switch strings.ToLower(filepath.Ext(output)) {
	case ".mp4", ".m4v", ".mov":
		subCodec = "mov_text"
	}

	args := muxArgs(input, srtPath, lang, title, output, subCodec, subCount)
	return runFFmpeg(exec.Command("ffmpeg", args...), progressReporter(input, onProgress))
}

// muxArgs builds the ffmpeg argument list that copies every stream from input
// and appends the translated SRT as the (subCount+1)-th subtitle track.
//
// subCount is the number of subtitle streams already in input, so it doubles as
// the 0-based index of the track being added.
func muxArgs(input, srtPath, lang, title, output, subCodec string, subCount int) []string {
	// Metadata specifier is -metadata:s:<stream_spec>, and the stream spec for
	// "the Nth subtitle stream" is itself "s:N" — hence the doubled s:s:.
	meta := fmt.Sprintf("-metadata:s:s:%d", subCount)

	// Matroska's Language element is ISO 639-2, so a 2-letter -to code has to be
	// widened: a track tagged "es" is invisible to language selectors expecting
	// "spa", and players report it as an unidentified language.
	lang = normLang(lang)

	return []string{
		"-y",
		"-i", input,
		"-i", srtPath,
		"-map", "0",
		"-map", "1",
		"-c", "copy",
		"-c:s", subCodec,
		meta, fmt.Sprintf("language=%s", lang),
		meta, fmt.Sprintf("title=%s", title),
		output,
	}
}

func DefaultOutputPath(input, lang string) string {
	ext := filepath.Ext(input)
	base := strings.TrimSuffix(input, ext)
	return base + "." + strings.ToUpper(lang) + ext
}

func DefaultSRTPath(input, lang string) string {
	ext := filepath.Ext(input)
	base := strings.TrimSuffix(input, ext)
	return base + "." + lang + ".srt"
}

// SliceWAV cuts [start, start+dur) out of a WAV without re-encoding. The seek is
// an input option so ffmpeg skips bytes rather than decoding and discarding them,
// which matters when this runs once per chunk over a feature film.
func SliceWAV(inWAV string, start, dur time.Duration, outWAV string, onProgress Progress) error {
	// dur is the total already — the chunk's own length — so unlike the other
	// three callers this needs no ContainerDuration probe to know it.
	var report func(time.Duration)
	if onProgress != nil {
		report = func(done time.Duration) { onProgress(done, dur) }
	}
	return runFFmpeg(exec.Command("ffmpeg", sliceWAVArgs(inWAV, start, dur, outWAV)...), report)
}

func sliceWAVArgs(inWAV string, start, dur time.Duration, outWAV string) []string {
	return []string{
		"-y",
		"-ss", fmt.Sprintf("%.3f", start.Seconds()),
		"-t", fmt.Sprintf("%.3f", dur.Seconds()),
		"-i", inWAV,
		"-c", "copy",
		outWAV,
	}
}

// WAVDuration reports how long an audio file runs, which is what the chunk plan
// is built from.
func WAVDuration(path string) (time.Duration, error) {
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "csv=p=0",
		path,
	).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe duration: %w", err)
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", strings.TrimSpace(string(out)), err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}
