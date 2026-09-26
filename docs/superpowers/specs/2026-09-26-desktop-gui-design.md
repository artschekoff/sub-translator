# Desktop GUI Design

**Date:** 2026-09-26
**Status:** Approved

## Goal

A desktop window for people who do not want a terminal. Drag a film onto it, see what audio and subtitle tracks it holds, pick one and a target language, and get subtitles — with progress visible while it works and errors that lead somewhere instead of just being red.

## Non-Goals

- Replacing the CLI. It stays the primary interface and keeps working unchanged.
- Batch processing, a queue, or a library view. One film at a time.
- Editing subtitles after they are produced.
- Code signing and notarization. The DMG ships unsigned; the README explains the Gatekeeper prompt.
- A settings UI for anything the CLI cannot already configure.

## Context

`sub-translator` is a Go CLI. It probes a video with ffprobe, gets subtitles either from an existing track or by transcribing the audio with whisper.cpp, translates them through the OpenAI API, and writes an `.srt` or muxes it back in. A `-fast` mode slices the film into ten-minute chunks so the opening is watchable within about a minute.

Two properties of the repository have been defended all along and must survive:

- `go.mod` has **zero third-party requires**.
- `make build-all` cross-compiles the CLI to five platforms from one machine, which only works without cgo.

## Approach

**Wails v2** for the window. It is stable (v3 was still `beta.25` in September 2026), actively released, and uses the operating system's own webview rather than bundling Chromium — a 10–20 MB binary instead of 150 MB. Crucially, `runtime.OnFileDrop` returns **absolute file paths**, which a browser cannot: a page that accepts a drop gets the file's contents, and copying three gigabytes to learn a path is not a design.

Rejected: Fyne (draws its own widgets; no better on the cgo question and further from the web-page model the owner wanted), a plain local web page (no file paths), `go-astilectron` (archived in 2024), Lorca (unmaintained, needs Chrome installed), `webview_go` (stale since 2024).

**The GUI drives the CLI as a subprocess.** This was the owner's decision over importing the packages directly, and it has a consequence worth stating plainly because it rescues an invariant:

> Because the GUI never imports `internal/...`, it does not need to be in the same Go module. It lives in `gui/` with its **own `go.mod`**, where Wails and its dependency tree are confined. The root module keeps its zero requires, and `make build-all` keeps cross-compiling the CLI from one machine.

Had the GUI imported the packages, Wails would have landed in the root `go.mod` and both properties would have been lost.

**They speak a machine protocol, not prose.** The obvious cost of a subprocess boundary is that progress and errors must be scraped out of human-readable output, and any rewording breaks the GUI silently. So the CLI gains an event stream: newline-delimited JSON on stderr, emitted only under a flag. Without that flag not one character of the current output changes.

This is the same pattern as `git status --porcelain`, `kubectl -o json` and — most aptly — `ffmpeg -progress`, which exists precisely so wrappers need not parse ffmpeg's human output. We already drive ffmpeg as a subprocess for the same reason.

It is deliberately **not** JSON-RPC. The stream is one-way: the GUI states its intent in argv and the CLI reports what happens. Bidirectional RPC earns its complexity when the child must *ask* something mid-run, and nothing here does — every decision is made before launch. If that changes, the upgrade path is the same events wrapped in JSON-RPC with a long-lived server mode.

## Part 1: CLI additions

Three additions, useful on their own for anyone scripting the tool.

### `-events json`

Writes newline-delimited JSON to **stderr**. stdout keeps carrying the human output unchanged, so `-events json` composes with everything else.

Every event has `type` and `t` (RFC 3339 timestamp). The first line of any run is always a `version` event, so a future change can be detected rather than guessed at:

```json
{"type":"version","t":"…","events":1,"tool":"1.4.0"}
{"type":"probe","t":"…","duration":8348.674,
 "audio":[{"index":3,"lang":"eng","title":"Original (Eng)","codec":"ac3"}],
 "subtitles":[]}
{"type":"stage","t":"…","name":"extract"}
{"type":"stage","t":"…","name":"transcribe","chunk":1,"of":14,"from":"0:00:00","to":"0:10:00"}
{"type":"progress","t":"…","pct":42}
{"type":"language","t":"…","detected":"en"}
{"type":"stage","t":"…","name":"translate","chunk":1,"of":14}
{"type":"saved","t":"…","path":"/…/movie.es.srt","covers":"0:10:00","watchable":true}
{"type":"warning","t":"…","kind":"blocks_untranslated","count":12,"total":1834,"first":[204,205,206]}
{"type":"done","t":"…","path":"/…/movie.es.srt","covers":"2:19:08"}
{"type":"error","t":"…","kind":"missing_key","message":"…"}
```

`progress` carries a percentage for the current stage. `saved` fires after every chunk in `-fast` mode, with `watchable` true from the first one.

### `-probe`

Prints the `version` and `probe` events and exits without doing any work. The GUI needs the track list before the user can choose anything, and today probing is welded to the start of a run.

With `-events json` it emits JSON; without it, the human-readable track listing the tool already prints. It requires only the input path — no `-to`, no key.

### Typed errors

Every fatal error gains a `kind` so a caller can act on it rather than match on wording. The message stays exactly as it is today.

| `kind` | Raised when |
|---|---|
| `missing_key` | no OpenAI key in the environment or the config |
| `whisper_not_found` | `whisper-cli` is neither configured nor on `PATH` |
| `no_model` | no ggml model configured or discovered |
| `ambiguous_model` | several models found, none chosen |
| `no_audio_track` | the file has no audio to transcribe |
| `no_subtitle_track` | `-source sub` on a file with none |
| `probe_failed` | ffprobe could not read the file |
| `rate_limited` | the translation endpoint refused everything |
| `transcript_empty` | the audio produced no speech |
| `cancelled` | SIGINT or SIGTERM |
| `other` | anything not yet classified |

The first four are what the GUI routes to its settings screen.

## Part 2: The window

### Structure

```
gui/
  go.mod              module github.com/artschekoff/sub-translator/gui
  main.go             Wails app, window lifecycle, OnFileDrop
  runner.go           spawns the CLI, decodes the event stream
  runner_test.go
  settings.go         reads and writes config through `sub-translator config`
  frontend/
    index.html
    app.css
    app.js
```

The frontend is plain HTML, CSS and JavaScript. No framework and no bundler: the whole UI is three screens and a progress bar, and a node toolchain would cost more than it returns.

### Finding the CLI

In order: a `sub-translator` binary beside the GUI executable (inside the `.app` bundle, which is how the DMG ships), then `$PATH`, then a path the user picked earlier.

That last one **cannot** live in the shared config, because the GUI reads that config by running the CLI — and needing the CLI to find out where the CLI is would be circular. It is the one piece of state the GUI owns itself, in `~/Library/Application Support/sub-translator/gui.json`, written only when the user picks a binary by hand.

Failing all three, the GUI opens on its settings screen saying it cannot find the tool, with a file picker — the same treatment as a missing key, and for the same reason: it is a setup step, not a run failure.

### Screens

**Drop.** An empty window inviting a film, with a "Choose…" button opening the native dialog for anyone who prefers it. `runtime.OnFileDrop` supplies absolute paths; a drop of more than one file takes the first and says so.

**File.** Filename and duration, the audio tracks and subtitle tracks as radio lists (index, language, title), a target-language picker, output mode (sidecar `.srt` / embed in a new container / both) with a save path, and a "fast mode" checkbox. Start is disabled until a track and a language are chosen.

Subtitle tracks and audio tracks are one choice, not two: picking an audio track means transcription, picking a subtitle track means translating what is already there. When the file has no subtitle tracks the section says so rather than showing an empty box, because that is the case the whole transcription feature exists for.

Fast mode is disabled unless the source is an audio track and the output is a sidecar `.srt`, mirroring what `validateFast` enforces, with a line saying why rather than a dead control.

**Run.** The current stage in words, a progress bar, and for `-fast` the growing coverage with a prominent note the moment the file is watchable. A Cancel button. On completion, the path, a "Show in Finder" button, and any warning about untranslated blocks — carried through from the `warning` event, because a file that is 12 blocks short of translated must not look identical to one that is not.

**Settings.** OpenAI key (masked, with a Show toggle), model, workers, whisper binary, whisper model, VAD model, chunk length. Every one of these reads and writes the same `~/.config/sub-translator/config.json` through `sub-translator config get` and `config set`, so the GUI holds no second copy of the validation rules and a value set in either interface is seen by the other.

The path to the CLI itself sits apart, above a divider, labelled as the one setting the window keeps for itself — for the bootstrap reason given above.

### Errors lead somewhere

An `error` event with `kind` of `missing_key`, `whisper_not_found`, `no_model` or `ambiguous_model` switches to Settings, focuses the field at fault, and shows the message above it. Every other kind stays on the Run screen with the message and a Retry button.

This is the owner's stated requirement: a missing key is not a failure to report, it is a setup step to complete.

### Cancelling

Cancel sends SIGINT to the child. The CLI already handles that: it removes its scratch directory and exits 130, leaving whatever chunks were already written intact. The GUI waits for the exit, and reports what the partial file covers when there is one.

Closing the window while work is running asks first — "transcription is at 34%, leaving now loses it" — and on confirmation cancels the child before quitting. One window, one lifetime, no background orphans.

## Packaging

`make gui` builds the `.app` with `wails build`. `make dmg` packs it with `hdiutil` into `bin/dist/sub-translator-<version>.dmg`, containing the bundle and a symlink to `/Applications`, with the CLI binary placed inside `Contents/MacOS/` so the GUI finds it first.

Both are macOS-only targets and neither is part of `make validate` or `make build-all`, which must keep working on a machine with no Wails and no cgo.

The DMG is unsigned. macOS will say the developer cannot be verified; the README explains the right-click-Open path and that signing needs an Apple Developer account. Adding signing later is a change to one target, not to the design.

## Testing

The GUI module's tests cover what is testable without a window:

- the event decoder against a recorded stream, including an unknown `type` (must be ignored, not fatal), a malformed line, and a truncated final line
- the `version` event's `events` number being refused when it is newer than the GUI understands
- CLI discovery order, and the settings-screen fallback when nothing is found
- argv construction from the form's state, for every combination of source, mode and fast
- error `kind` → screen routing, including an unrecognised kind falling back to the Run screen
- cancellation: a stub CLI that traps SIGINT, asserting the GUI waits for the exit rather than orphaning it

The CLI's own additions are tested in the root module: the event stream's shape for a full run against the existing stub binaries, `-probe` exiting without side effects, that `-events json` leaves stdout byte-identical to a run without it, and that every error path carries a `kind`.

Manual acceptance, on a real film: drag it in, see the three audio tracks, pick the English one and Spanish, tick fast mode, watch the first chunk land, open the file in a player while the rest continues, and cancel a run mid-transcription to confirm nothing is left in `$TMPDIR`.

## Constraints

- The root `go.mod` keeps **zero third-party requires**; Wails lives only in `gui/go.mod`.
- `make validate` and `make build-all` must work unchanged on a machine without Wails, cgo or a webview.
- Without `-events json` the CLI's output does not change by one character.
- `-probe` does no work and needs no API key.
- The GUI holds no copy of the configuration rules — every read and write goes through the CLI, except the bootstrap path to the CLI binary itself.
- Go 1.26 floor. Conventional commits.
