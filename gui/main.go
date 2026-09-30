// Command gui is the desktop window for sub-translator. It drives the CLI
// binary as a subprocess through its JSON Lines event stream on stderr; it
// never imports the CLI's packages (see module_test.go).
//
// main.go owns the window: its options, its lifecycle, the file drop, and the
// methods the page calls over the Wails binding. Everything those methods do
// lives elsewhere — locate.go finds the CLI, runner.go runs it, settings.go
// reads and writes the shared config, rules.go answers the two questions the
// File screen's controls ask. This file wires them to a window and does no
// deciding of its own.
//
// Platform scope. The window's options below configure macOS and Windows both,
// because Wails puts the translucency flags on a per-platform struct and there
// is no top-level place to set them. That is not a claim that the app is
// cross-platform: `make gui` and `make dmg` are macOS-only targets, the DMG is
// how the CLI ships beside the GUI executable, and statePath() in guistate.go
// writes to ~/Library/Application Support on every platform — see its comment.
// The Windows block is the honest setting for the one platform whose flags had
// to be named, compile-verified only, exactly like the CLI's own
// internal/proc KillGroup.
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// The three files are named one by one rather than embedded as a directory,
// and both halves of that matter.
//
// `//go:embed frontend` does not mean "the three files this window ships". It
// means everything under frontend/ except path elements beginning with "." or
// "_" — and `wails build` generates frontend/wailsjs/ there, gitignored, with
// its own package.json inside it. That directory is neither dot- nor
// underscore-prefixed, so the directory form embeds it: nothing here loads it,
// but it ships inside the binary, putting a package.json in the artifact of a
// project whose constraints forbid a node toolchain.
// TestTheTrackedTreeHasNoNodeToolchain cannot see that, and is right not to —
// it guards the tracked tree, which is a different surface — so
// TestTheEmbeddedAssetsHaveNoNodeToolchain guards this one.
//
// Naming the files also makes the embedded set deterministic. With the
// directory form, what ships depends on whether `wails build` has ever run on
// the machine doing the building: three files on a clean clone, nine on a
// machine that has built once. An artifact that varies with the builder's
// history is not reproducible from the tracked tree.
//
// The "." / "_" rule still applies to what is named here, so a future asset
// must keep a plain name or take an "all:" prefix of its own: without one it
// would build cleanly and simply be missing at runtime.
//
//go:embed frontend/index.html frontend/app.css frontend/app.js
var embeddedAssets embed.FS

// App is the object the page talks to. Every exported method on it becomes
// window.go.main.App.<Name> in the frontend.
//
// The three mutable fields are guarded because Wails serves each binding call
// from the webview's message pump and OnFileDrop from its own callback: a drop
// arriving while the page is asking about fast mode is two goroutines on the
// same probe.
type App struct {
	ctx context.Context

	mu     sync.Mutex
	cli    string
	cliErr error
	film   string
	probe  *Probe
}

// startup runs once, when the window has a context to talk through. It finds
// the CLI and arms the file drop; it deliberately does no probing and shows no
// dialog, so the window appears immediately.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	cli, err := LocateCLI()
	a.mu.Lock()
	a.cli, a.cliErr = cli, err
	a.mu.Unlock()

	// The callback's coordinates come first: OnFileDrop takes
	// func(x, y int, paths []string), not func(paths []string). The window has
	// one drop target — the whole card — so where the file landed tells it
	// nothing it does not already know.
	wailsruntime.OnFileDrop(ctx, func(_, _ int, paths []string) {
		a.onFileDrop(paths)
	})
}

// Dropped is what the page receives when a film is dropped on the window.
//
// Ignored is how many of the dropped files were not used. A drop of three
// films takes the first, and the window says the other two were ignored:
// silently dropping them is how someone ends up watching a translation of the
// wrong film and cannot work out why.
type Dropped struct {
	Path    string `json:"path"`
	Ignored int    `json:"ignored"`
}

// onFileDrop forwards a drop to the page as an event rather than probing here,
// so a dropped file and a file chosen through the dialog take exactly the same
// path afterwards: the page calls ProbeFilm either way.
//
// It emits on every drop, including one that carried nothing this window can
// use, and that is not defensiveness for its own sake. A drag of text, or of
// an image from another application, still reaches the native view's
// performDragOperation; because DisableWebViewDrop is set, that method returns
// without letting a DOM drop event through, so this event is the page's only
// notice that the drag ended. Staying silent here left the dropzone's
// highlight on until the next drag.
//
// Empty paths are filtered rather than counted. The native side joins the file
// URLs it found with newlines and the dispatcher splits on them, so a drag
// with no file URLs at all arrives as a one-element slice holding "" — not as
// an empty slice — and counting that would report one file dropped and then
// show nothing.
func (a *App) onFileDrop(paths []string) {
	usable := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			usable = append(usable, path)
		}
	}

	dropped := Dropped{}
	if len(usable) > 0 {
		dropped.Path = usable[0]
		dropped.Ignored = len(usable) - 1
	}
	wailsruntime.EventsEmit(a.ctx, "film:dropped", dropped)
}

// BootInfo is what the window needs to know before it can show anything: where
// the CLI is, or that there isn't one.
//
// Found false is a setup step, not a run failure — the spec's Finding the CLI
// section is explicit that it gets the same treatment as a missing key — so the
// page shows a picker rather than an error dialog.
type BootInfo struct {
	CLI   string `json:"cli"`
	Found bool   `json:"found"`
	Error string `json:"error"`
}

// Boot reports the CLI lookup's result. The page calls it first.
func (a *App) Boot() BootInfo {
	a.mu.Lock()
	cli, err := a.cli, a.cliErr
	a.mu.Unlock()

	info := BootInfo{CLI: cli, Found: err == nil}
	switch {
	case err == nil:
	case errors.Is(err, ErrCLINotFound):
		info.Error = "sub-translator is not beside this app and not on your PATH. " +
			"Point the window at the binary to continue."
	default:
		info.Error = err.Error()
	}
	return info
}

// ChooseCLI asks for the sub-translator binary by hand and remembers it for
// future launches. It is the third and last of LocateCLI's strategies, and the
// only one a person drives.
func (a *App) ChooseCLI() (BootInfo, error) {
	path, err := wailsruntime.OpenFileDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title:           "Choose the sub-translator binary",
		ResolvesAliases: true,
	})
	if err != nil {
		return a.Boot(), err
	}
	if path == "" {
		// Cancelled. Not an error, and nothing changes.
		return a.Boot(), nil
	}

	if err := RememberCLI(path); err != nil {
		return a.Boot(), err
	}

	// Re-run the whole lookup rather than trusting the chosen path: LocateCLI
	// also checks that what was picked is a regular file with an execute bit,
	// and a user can pick a .txt in an open dialog.
	cli, lookupErr := LocateCLI()
	a.mu.Lock()
	a.cli, a.cliErr = cli, lookupErr
	a.mu.Unlock()
	return a.Boot(), nil
}

// ChooseFilm opens the native open dialog and returns the chosen path, or ""
// when the dialog was cancelled. It is the "Choose…" button on the Drop
// screen, for anyone who would rather not drag.
func (a *App) ChooseFilm() (string, error) {
	return wailsruntime.OpenFileDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title:           "Choose a film",
		ResolvesAliases: true,
		Filters: []wailsruntime.FileFilter{
			{
				DisplayName: "Video files",
				// The containers the CLI's own help text names, plus the ones
				// ffprobe reads anyway. A filter that is too narrow hides a
				// file the CLI would have handled, so "All files" stays.
				Pattern: "*.mkv;*.mp4;*.m4v;*.mov;*.avi;*.webm;*.ts;*.mpg;*.mpeg;*.wmv;*.flv",
			},
			{DisplayName: "All files", Pattern: "*"},
		},
	})
}

// FileInfo is everything the File screen shows about one film. The track lists
// are the probe's own, verbatim — the window renders them and adds nothing.
//
// Modes and ModesNote are OutputModes' answer, computed here rather than in the
// page: see rules.go for why both of the File screen's decisions are Go.
type FileInfo struct {
	Path      string       `json:"path"`
	Name      string       `json:"name"`
	Container string       `json:"container"`
	Duration  float64      `json:"duration"`
	Audio     []Stream     `json:"audio"`
	Subtitles []Stream     `json:"subtitles"`
	Modes     []OutputMode `json:"modes"`
	ModesNote string       `json:"modesNote"`
}

// ProbeFilm runs the CLI's -probe on path and returns what the File screen
// shows. It is the only place in the window that turns a file into tracks.
//
// A ProbeFailure — the CLI's own error event, most often kind "probe_failed"
// for a file ffprobe cannot read — is surfaced with the CLI's message
// verbatim. The window has no better wording for it and inventing "no tracks"
// would be a lie about a file that may be fine.
func (a *App) ProbeFilm(path string) (*FileInfo, error) {
	a.mu.Lock()
	cli := a.cli
	a.mu.Unlock()
	if cli == "" {
		return nil, errors.New("sub-translator has not been found yet")
	}

	probe, err := ProbeFile(a.ctx, cli, path)
	if err != nil {
		var failure *ProbeFailure
		switch {
		case errors.As(err, &failure):
			return nil, errors.New(failure.Message)
		case errors.Is(err, ErrCancelled):
			return nil, errors.New("the probe was cancelled")
		}
		return nil, err
	}

	a.mu.Lock()
	a.film, a.probe = path, probe
	a.mu.Unlock()

	modes, note := OutputModes(probe)
	return &FileInfo{
		Path:      path,
		Name:      filepath.Base(path),
		Container: container(path),
		Duration:  probe.Duration,
		Audio:     probe.Audio,
		Subtitles: probe.Subtitles,
		Modes:     modes,
		ModesNote: note,
	}, nil
}

// container is the label beside the duration: the file's extension, upper
// case, without the dot. It is presentation only — nothing is decided from it,
// because what a container can actually hold is what probe.embeds_subtitles is
// for (see OutputModes).
func container(path string) string {
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	return strings.ToUpper(ext)
}

// FastMode answers the fast-mode checkbox for the source and output the form
// currently holds, by calling FastAvailable with the probe this window last
// took. The rule itself is in rules.go, table-tested; this is the binding.
func (a *App) FastMode(source, mode string) FastState {
	a.mu.Lock()
	probe := a.probe
	a.mu.Unlock()

	available, reason := FastAvailable(probe, source, mode)
	return FastState{Available: available, Reason: reason}
}

// SourceLang answers whether the chosen track needs a source language, and
// what to prefill the field with. See SourceLanguage in rules.go, and the
// spec's File screen paragraph, for why this control exists at all.
func (a *App) SourceLang(source string, track int) SourceLangState {
	a.mu.Lock()
	probe := a.probe
	a.mu.Unlock()

	needed, prefill := SourceLanguage(probe, source, track)
	return SourceLangState{Needed: needed, Prefill: prefill}
}

// ChooseOutput opens the native save dialog for the run's output path,
// starting in the film's own directory, and returns "" when cancelled.
//
// It suggests no filename on purpose. sub-translator derives its own default
// (media.DefaultSRTPath and media.DefaultOutputPath), and the window does not
// report a path the CLI never promised — an empty -out means the CLI names the
// file, which is what the File screen says until someone chooses otherwise.
func (a *App) ChooseOutput() (string, error) {
	a.mu.Lock()
	film := a.film
	a.mu.Unlock()

	opts := wailsruntime.SaveDialogOptions{
		Title:                "Save the translation as",
		CanCreateDirectories: true,
	}
	if film != "" {
		opts.DefaultDirectory = filepath.Dir(film)
	}
	return wailsruntime.SaveFileDialog(a.ctx, opts)
}

func main() {
	assets, err := fs.Sub(embeddedAssets, "frontend")
	if err != nil {
		log.Fatal(err)
	}

	app := &App{}

	err = wails.Run(&options.App{
		Title: "sub-translator",
		// One size for every screen. The spec is explicit that the window
		// never changes shape between Drop, File, Run and Settings — that is
		// what keeps it one surface instead of a wizard — so nothing here or
		// in app.js ever calls WindowSetSize, and no screen sets a height.
		// The File screen's track list scrolls inside its card instead.
		Width:  520,
		Height: 560,
		// Wails fills this with opaque white when it is nil
		// (pkg/options/options.go:171-178), and an opaque window background
		// defeats a transparent webview: the blur never shows, with no error
		// to explain it. This is the line that makes the glass real rather
		// than a CSS imitation of it.
		BackgroundColour: &options.RGBA{R: 0, G: 0, B: 0, A: 0},
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		DragAndDrop: &options.DragAndDrop{
			// Without this the native view hands drops straight to WebKit and
			// OnFileDrop never fires (darwin/window.go:88).
			EnableFileDrop: true,
			// And with it alone, WailsWebView.m still calls through to
			// super's performDragOperation, so WebKit *also* handles the drop
			// and navigates the page to the dropped film — the window
			// replaces itself with a video player. Disabling the webview's own
			// drop is what stops that; Wails still delivers the paths.
			DisableWebViewDrop: true,
		},
		Mac: &mac.Options{
			// The compositor's own blur. This pair is the documented way to a
			// frosty window; a CSS backdrop-filter over an opaque page is the
			// imitation of it. Both flags live here rather than on
			// options.App, where they do not exist.
			WindowIsTranslucent:  true,
			WebviewIsTransparent: true,
			// A seamless top edge: the page runs under the title bar, so the
			// blur shows through it.
			TitleBar:   &mac.TitleBar{TitlebarAppearsTransparent: true, FullSizeContent: true},
			Appearance: mac.DefaultAppearance,
		},
		Windows: &windows.Options{
			WindowIsTranslucent:  true,
			WebviewIsTransparent: true,
			BackdropType:         windows.Mica,
		},
		OnStartup: app.startup,
		Bind:      []interface{}{app},
	})
	if err != nil {
		log.Fatal(fmt.Errorf("gui: %w", err))
	}
}
