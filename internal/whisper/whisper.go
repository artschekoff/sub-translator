// Package whisper runs the external whisper.cpp CLI to turn an audio file into
// an SRT transcript. The binary is invoked as a subprocess for the same reason
// ffmpeg is: it keeps the project free of cgo, so `make build-all` can still
// cross-compile to every platform from one machine.
package whisper

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/artschekoff/sub-translator/internal/proc"
)

type Options struct {
	Bin      string // path to whisper-cli
	Model    string // path to the ggml transcription model
	VADModel string // path to a Silero VAD model; empty disables VAD
	Audio    string // 16 kHz mono WAV
	OutBase  string // output path WITHOUT extension; whisper appends .srt and .json
	Language string // ISO 639-1 code, or empty/"auto" to detect

	VADThreshold float64 // 0 leaves whisper's own default in place
	Threads      int     // 0 lets whisper choose
}

type Result struct {
	Language string // the language actually used, detected when Language was auto
	SRTPath  string // the transcript whisper wrote
}

// candidates are the names whisper.cpp has shipped its CLI under. Homebrew's
// formula installs "whisper-cli"; older builds and some distributions use
// "whisper-cpp".
var candidates = []string{"whisper-cli", "whisper-cpp"}

// FindBinary resolves the whisper executable: an explicitly configured path
// first, then PATH. The error is written to be actionable, since "executable
// file not found in $PATH" tells a user nothing about what to install.
func FindBinary(configured string) (string, error) {
	if c := strings.TrimSpace(configured); c != "" {
		if _, err := os.Stat(c); err != nil {
			return "", fmt.Errorf("whisper binary not found at %s "+
				"(set another with: sub-translator config set whisper.bin <path>)", c)
		}
		return c, nil
	}
	for _, name := range candidates {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("whisper-cli not found — install it (brew install whisper.cpp) " +
		"or point at it: sub-translator config set whisper.bin <path>")
}

// Args builds the whisper-cli argument list. It is pure so the entire flag
// contract can be tested without the binary installed.
func Args(o Options) []string {
	lang := strings.TrimSpace(o.Language)
	if lang == "" {
		lang = "auto"
	}

	args := []string{
		"-m", o.Model,
		"-f", o.Audio,
		"-of", o.OutBase,
		"-l", lang,
		"-osrt",
		"-oj",
		"-pp",
	}
	if o.Threads > 0 {
		args = append(args, "-t", strconv.Itoa(o.Threads))
	}
	// --vad is meaningless without a model, and whisper-cli exits if given one
	// without the other.
	if strings.TrimSpace(o.VADModel) != "" {
		args = append(args, "--vad", "-vm", o.VADModel)
		if o.VADThreshold > 0 {
			args = append(args, "-vt", strconv.FormatFloat(o.VADThreshold, 'g', -1, 64))
		}
	}
	return args
}

var progressRE = regexp.MustCompile(`progress\s*=\s*(\d+)%`)

func parseProgress(line string) (int, bool) {
	m := progressRE.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// segmentRE matches the line whisper-cli prints to stdout for every segment it
// has just recognised: "[00:00:01.000 --> 00:00:04.000]  text".
var segmentRE = regexp.MustCompile(`^\[\d+:\d\d:\d\d[.,]\d+ --> \d+:\d\d:\d\d[.,]\d+\]\s*(.*)$`)

// parseSegment returns the text of one stdout segment line. A segment with no
// text is not one: there is nothing in it to show.
func parseSegment(line string) (string, bool) {
	m := segmentRE.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return "", false
	}
	text := strings.TrimSpace(m[1])
	return text, text != ""
}

// readDetectedLanguage reads the language out of whisper's JSON sidecar. The
// file is a convenience, not the deliverable, so anything unreadable falls back
// to the language that was requested.
func readDetectedLanguage(outBase, requested string) string {
	data, err := os.ReadFile(outBase + ".json")
	if err != nil {
		return requested
	}
	var doc struct {
		Result struct {
			Language string `json:"language"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return requested
	}
	if lang := strings.TrimSpace(doc.Result.Language); lang != "" {
		return lang
	}
	return requested
}

// Run transcribes o.Audio and returns the SRT path plus the language used.
// Progress is reported as whole percentages as whisper emits them, and segment,
// when not nil, is handed the text of every segment the moment whisper prints
// it — the SRT itself is only written once the whole file is done, which is far
// too late to show anyone what is being heard.
func Run(o Options, progress func(pct int), segment func(text string)) (Result, error) {
	cmd := exec.Command(o.Bin, Args(o)...)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("whisper: %w", err)
	}
	var stdout io.Reader
	if segment != nil {
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return Result{}, fmt.Errorf("whisper: %w", err)
		}
		stdout = pipe
	} else {
		cmd.Stdout = io.Discard
	}

	// A transcription is the longest thing this program starts, so it is the
	// child most worth being able to kill: proc.Start gives it a process group
	// of its own, so it goes when the run is cancelled instead of transcribing
	// on alone.
	if err := proc.Start(cmd); err != nil {
		return Result{}, fmt.Errorf("whisper: %w", err)
	}

	// stdout is read on its own goroutine: whisper writes both streams as it
	// goes, and a pipe nobody drains blocks the child once its buffer fills. It
	// must be fully read before proc.Wait, which closes the pipe.
	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		if stdout == nil {
			return
		}
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if text, ok := parseSegment(sc.Text()); ok {
				segment(text)
			}
		}
		io.Copy(io.Discard, stdout)
	}()

	// Keep the last few lines: when whisper fails, the reason is at the end of
	// its output, and dumping thousands of lines of model chatter helps nobody.
	var tail []string
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if pct, ok := parseProgress(line); ok {
			if progress != nil {
				progress(pct)
			}
			continue
		}
		if strings.TrimSpace(line) != "" {
			tail = append(tail, line)
			if len(tail) > 10 {
				tail = tail[1:]
			}
		}
	}

	// Check for scanner errors (e.g., stderr line longer than buffer). If the
	// scanner hit an error, we must drain the pipe to prevent cmd.Wait() from
	// deadlocking on a full buffer.
	scanErr := scanner.Err()
	if scanErr != nil {
		io.Copy(io.Discard, stderr)
	}
	<-stdoutDone

	// proc.Wait, not cmd.Wait: it drops the reaped child from the set of process
	// groups a cancel kills. Every path below this point has already been through
	// it, and there is no path from proc.Start to here that skips it.
	if err := proc.Wait(cmd); err != nil {
		if scanErr != nil {
			// Include both the command error and the stderr read error
			err = fmt.Errorf("%w (stderr read: %v)", err, scanErr)
		}
		// A silent failure leaves the tail empty, and appending it then adds a
		// trailing blank line where a diagnostic should be.
		if t := strings.Join(tail, "\n"); t != "" {
			return Result{}, fmt.Errorf("whisper: %w\n%s", err, t)
		}
		return Result{}, fmt.Errorf("whisper: %w", err)
	}

	srtPath := o.OutBase + ".srt"
	if _, err := os.Stat(srtPath); err != nil {
		// If there was a scanner error and no transcript was produced, report both
		if scanErr != nil {
			return Result{}, fmt.Errorf("whisper produced no transcript at %s (stderr read: %v)\n%s",
				srtPath, scanErr, strings.Join(tail, "\n"))
		}
		return Result{}, fmt.Errorf("whisper produced no transcript at %s\n%s",
			srtPath, strings.Join(tail, "\n"))
	}
	return Result{
		Language: readDetectedLanguage(o.OutBase, o.Language),
		SRTPath:  srtPath,
	}, nil
}
