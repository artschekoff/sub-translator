package media

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func ExtractSubtitle(input string, streamIndex int, outSRT string) error {
	cmd := exec.Command("ffmpeg", "-y",
		"-i", input,
		"-map", fmt.Sprintf("0:%d", streamIndex),
		outSRT,
	)
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
func ExtractAudio(input string, streamIndex int, outWAV string) error {
	cmd := exec.Command("ffmpeg", extractAudioArgs(input, streamIndex, outWAV)...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
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

func MuxSubtitle(input, srtPath, lang, title, output string) error {
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
	cmd := exec.Command("ffmpeg", args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
