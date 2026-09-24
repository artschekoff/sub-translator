// Package models knows where whisper.cpp ggml models live, which ones can be
// downloaded, and how to fetch one. It deliberately shares the upstream
// ggml-<name>.bin naming so models downloaded by other whisper.cpp front-ends
// are found and reused rather than duplicated.
package models

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Kind int

const (
	// KindTranscribe is a Whisper speech-recognition model, passed to -m.
	KindTranscribe Kind = iota
	// KindVAD is a voice-activity-detection model, passed to --vad-model.
	KindVAD
)

func (k Kind) String() string {
	if k == KindVAD {
		return "vad"
	}
	return "transcribe"
}

type Model struct {
	Name     string
	Filename string
	URL      string
	Kind     Kind
}

const (
	whisperBase = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/"
	vadBase     = "https://huggingface.co/ggml-org/whisper-vad/resolve/main/"
)

// whisperModels are the upstream names worth offering. Quantised and
// English-only variants exist upstream too, but every extra row is a row to
// keep correct, and these cover the useful quality/size range.
var whisperModels = []string{
	"tiny", "base", "small", "medium", "large-v2", "large-v3", "large-v3-turbo",
}

func Catalog() []Model {
	out := make([]Model, 0, len(whisperModels)+1)
	for _, name := range whisperModels {
		filename := "ggml-" + name + ".bin"
		out = append(out, Model{
			Name:     name,
			Filename: filename,
			URL:      whisperBase + filename,
			Kind:     KindTranscribe,
		})
	}
	const vadFile = "ggml-silero-v5.1.2.bin"
	out = append(out, Model{
		Name:     "silero-vad",
		Filename: vadFile,
		URL:      vadBase + vadFile,
		Kind:     KindVAD,
	})
	return out
}

func Find(name string) (Model, bool) {
	name = strings.TrimSpace(strings.ToLower(name))
	for _, m := range Catalog() {
		if m.Name == name {
			return m, true
		}
	}
	return Model{}, false
}

// DefaultDir is where `model pull` writes. It is ours alone; models found in
// other applications' directories are read but never written.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "sub-translator", "models"), nil
	}
	if base := os.Getenv("XDG_DATA_HOME"); base != "" {
		return filepath.Join(base, "sub-translator", "models"), nil
	}
	return filepath.Join(home, ".local", "share", "sub-translator", "models"), nil
}

// SearchDirs lists the directories scanned for an already-present model, in
// precedence order. Directories that do not exist are harmless: Discover skips
// them.
func SearchDirs(configuredDir string) []string {
	var dirs []string
	if strings.TrimSpace(configuredDir) != "" {
		dirs = append(dirs, configuredDir)
	}
	if own, err := DefaultDir(); err == nil {
		dirs = append(dirs, own)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if runtime.GOOS == "darwin" {
			// Slaive is a macOS whisper.cpp front-end. Reading its models saves
			// re-downloading well over a gigabyte.
			dirs = append(dirs, filepath.Join(home,
				"Library", "Application Support", "net.slaive.app", "models"))
		}
		dirs = append(dirs, filepath.Join(home, ".cache", "whisper.cpp"))
	}
	return append(dirs, "models")
}

// IsVADFile reports whether a ggml file is a voice-activity-detection model.
// Passing one to -m fails deep inside whisper with an unhelpful message, so the
// two kinds are separated by name before they ever reach the binary.
func IsVADFile(name string) bool {
	lower := strings.ToLower(filepath.Base(name))
	return strings.Contains(lower, "silero") || strings.Contains(lower, "vad")
}

// Discover scans dirs for ggml models and splits them by kind. Paths are
// returned in the order their directories were given, so the caller's
// precedence survives.
func Discover(dirs []string) (transcribe, vad []string) {
	seen := map[string]bool{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasPrefix(name, "ggml-") || !strings.HasSuffix(name, ".bin") {
				continue
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			path := filepath.Join(dir, name)
			if IsVADFile(name) {
				vad = append(vad, path)
			} else {
				transcribe = append(transcribe, path)
			}
		}
	}
	return transcribe, vad
}

// Pull downloads m into destDir and returns the saved path. It writes to a
// .part file and renames on success, so an interrupted download can never be
// mistaken for a usable model by Discover.
func Pull(m Model, destDir string, progress func(done, total int64)) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", destDir, err)
	}
	final := filepath.Join(destDir, m.Filename)

	resp, err := http.Get(m.URL)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", m.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", m.Name, resp.Status)
	}

	f, err := os.CreateTemp(destDir, m.Filename+".*.part")
	if err != nil {
		return "", fmt.Errorf("create %s: %w", m.Filename+".part", err)
	}
	tmp := f.Name()
	total := resp.ContentLength
	written, err := io.Copy(f, &progressReader{r: resp.Body, total: total, report: progress})
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil && total > 0 && written != total {
		err = fmt.Errorf("download %s: got %d bytes, expected %d", m.Name, written, total)
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("finalize %s: %w", final, err)
	}
	return final, nil
}

type progressReader struct {
	r      io.Reader
	done   int64
	total  int64
	report func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.report != nil && n > 0 {
		p.report(p.done, p.total)
	}
	return n, err
}
