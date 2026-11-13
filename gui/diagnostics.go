package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// DiagnosticAction IDs the frontend can trigger for a failing check.
const (
	ActionChooseCLI          = "choose-cli"
	ActionInstallFFmpeg      = "install-ffmpeg"
	ActionChooseFFmpeg       = "choose-ffmpeg"
	ActionSettingsWhisperBin = "settings-whisper-bin"
	ActionPullModel          = "pull-model"
	ActionSettingsModelsDir  = "settings-models-dir"
	ActionOpenAccessibility  = "open-accessibility"
)

// DiagnosticCheck is one row in the launch wizard.
type DiagnosticCheck struct {
	Name    string   `json:"name"`
	Label   string   `json:"label"`
	OK      bool     `json:"ok"`
	Message string   `json:"message"`
	Actions []string `json:"actions"`
}

// DiagnosticsResult is the whole wizard state.
type DiagnosticsResult struct {
	OK     bool              `json:"ok"`
	Checks []DiagnosticCheck `json:"checks"`
}

// Diagnostics runs every launch-time check and returns a result the wizard
// screen renders. It is called on every boot and on every recheck.
func (a *App) Diagnostics() DiagnosticsResult {
	checks := []DiagnosticCheck{
		a.checkCLI(),
		a.checkFFmpeg(),
		a.checkWhisper(),
		a.checkModel(),
		a.checkPermissions(),
	}
	ok := true
	for _, c := range checks {
		if !c.OK {
			ok = false
			break
		}
	}
	return DiagnosticsResult{OK: ok, Checks: checks}
}

func (a *App) checkCLI() DiagnosticCheck {
	c := DiagnosticCheck{Name: "cli", Label: "sub-translator binary"}
	cli, err := LocateCLI()
	if err == nil && cli != "" {
		c.OK = true
		c.Message = cli
		return c
	}
	c.Message = "sub-translator binary not found beside the app or on PATH."
	c.Actions = []string{ActionChooseCLI}
	return c
}

func (a *App) checkFFmpeg() DiagnosticCheck {
	c := DiagnosticCheck{Name: "ffmpeg", Label: "ffmpeg & ffprobe"}
	missing := []string{}
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) == 0 {
		c.OK = true
		c.Message = "ffmpeg and ffprobe are available."
		return c
	}
	c.Message = fmt.Sprintf("%s not found in PATH.", strings.Join(missing, " and "))
	c.Actions = []string{ActionInstallFFmpeg, ActionChooseFFmpeg}
	return c
}

func (a *App) checkWhisper() DiagnosticCheck {
	c := DiagnosticCheck{Name: "whisper", Label: "whisper-cli binary"}

	// A whisper-cli on PATH is the happy path.
	if _, err := exec.LookPath("whisper-cli"); err == nil {
		c.OK = true
		c.Message = "whisper-cli found on PATH."
		return c
	}

	// Otherwise see if the CLI has a configured whisper.bin that exists.
	cli, err := LocateCLI()
	if err == nil && cli != "" {
		out, err := exec.CommandContext(context.Background(), cli, "config", "get", "whisper.bin").Output()
		if err == nil {
			if bin := strings.TrimSpace(string(out)); bin != "" {
				if info, err := os.Stat(bin); err == nil && !info.IsDir() {
					c.OK = true
					c.Message = bin
					return c
				}
			}
		}
	}

	c.Message = "whisper-cli not found. Install whisper.cpp or set whisper.bin in Settings."
	c.Actions = []string{ActionSettingsWhisperBin}
	return c
}

func (a *App) checkModel() DiagnosticCheck {
	c := DiagnosticCheck{Name: "model", Label: "Whisper model"}
	cli, err := LocateCLI()
	if err != nil || cli == "" {
		c.Message = "Cannot check models until the sub-translator binary is found."
		c.Actions = []string{ActionChooseCLI}
		return c
	}

	out, err := exec.CommandContext(context.Background(), cli, "model", "list").CombinedOutput()
	if err != nil {
		c.Message = "Could not run `sub-translator model list`: " + strings.TrimSpace(string(out))
		return c
	}
	if strings.Contains(string(out), "installed:") {
		c.OK = true
		c.Message = "At least one Whisper model is installed."
		return c
	}
	c.Message = "No Whisper model found. Download one or point models.dir to an existing model folder."
	c.Actions = []string{ActionPullModel, ActionSettingsModelsDir}
	return c
}

func (a *App) checkPermissions() DiagnosticCheck {
	c := DiagnosticCheck{Name: "permissions", Label: "macOS permissions"}
	if runtime.GOOS != "darwin" {
		c.OK = true
		c.Message = "Not applicable."
		return c
	}
	if accessibilityTrusted() {
		c.OK = true
		c.Message = "Accessibility access is granted."
		return c
	}
	c.Message = "Accessibility access is not granted. Some drag-and-drop or automation features may not work."
	c.Actions = []string{ActionOpenAccessibility}
	return c
}

// ChooseFFmpeg asks for the ffmpeg binary, remembers its directory, and
// re-applies PATH so the next Diagnostics call sees it.
func (a *App) ChooseFFmpeg() (DiagnosticsResult, error) {
	path, err := wailsruntime.OpenFileDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title:           "Choose the ffmpeg binary",
		ResolvesAliases: true,
	})
	if err != nil {
		return a.Diagnostics(), err
	}
	if path == "" {
		return a.Diagnostics(), nil
	}
	if err := RememberFFmpegDir(filepath.Dir(path)); err != nil {
		return a.Diagnostics(), err
	}
	ensurePATH()
	return a.Diagnostics(), nil
}

// InstallFFmpeg opens the Homebrew formula page so the user can install it.
func (a *App) InstallFFmpeg() {
	wailsruntime.BrowserOpenURL(a.ctx, "https://formulae.brew.sh/formula/ffmpeg")
}

// OpenAccessibilitySettings opens System Settings to the Accessibility privacy pane.
func (a *App) OpenAccessibilitySettings() {
	_ = exec.Command("open", "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility").Start()
}

// pullingModel is true while a model download is in progress.
var pullingModel atomic.Bool

// StartModelPull begins downloading name in the background and emits
// wizard:pull:line events with progress text and a wizard:pull:done event
// when it finishes. name defaults to large-v3-turbo.
func (a *App) StartModelPull(name string) error {
	if pullingModel.Swap(true) {
		return errors.New("a model download is already running")
	}
	if name == "" {
		name = "large-v3-turbo"
	}
	cli, err := LocateCLI()
	if err != nil || cli == "" {
		pullingModel.Store(false)
		return errors.New("sub-translator binary not found")
	}

	go a.runModelPull(cli, name)
	return nil
}

func (a *App) runModelPull(cli, name string) {
	defer pullingModel.Store(false)

	cmd := exec.CommandContext(context.Background(), cli, "model", "pull", name)
	var stderr bytes.Buffer
	pipe, pipeErr := cmd.StderrPipe()
	if pipeErr != nil {
		cmd.Stderr = &stderr
	} else {
		// Stream stderr lines as they arrive so the wizard can show progress.
		go func() {
			scan := bytes.NewBuffer(nil)
			buf := make([]byte, 1024)
			for {
				n, err := pipe.Read(buf)
				if n > 0 {
					scan.Write(buf[:n])
					for {
						line, rest, ok := bytes.Cut(scan.Bytes(), []byte("\n"))
						if !ok {
							break
						}
						a.send("wizard:pull:line", string(line))
						scan.Reset()
						scan.Write(rest)
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}

	err := cmd.Run()
	var msg string
	if err != nil {
		msg = strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
	}
	a.send("wizard:pull:done", msg)
}
