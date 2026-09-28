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

stderr already carries other things, and the design has to say what a consumer does about them: the tool's own `warning:` lines, the one `error:` line, `flag.Usage`'s text and the interactive prompts all share that stream. **A line that does not parse as JSON is human prose and must be skipped, not treated as a protocol error.** That is the rule, and it is only safe because every line the tool writes is whole. Two things are needed for that, not one: the event emitter and the prose writes share a lock, so neither can land inside the other's bytes; and because a lock says nothing about column position — an interactive prompt ends without a newline, since the answer is typed on the same line — the emitter also breaks an unfinished line before writing an event. The ffmpeg children — which write carriage-return-terminated progress with no newline at all, and so could put an event inside a line no line-oriented decoder could read — have their stderr captured instead of inherited, the way `whisper.Run` always did. A failing ffmpeg's diagnostics travel in the error message rather than on the stream.

Every event has `type` and `t` (RFC 3339 timestamp). The first line of any run is always a `version` event, so a future change can be detected rather than guessed at — including on an argv this CLI cannot parse, which is the case the negotiation exists for:

```json
{"type":"version","t":"…","events":1,"tool":"1.4.0"}
{"type":"probe","t":"…","duration":8348.674,
 "audio":[{"index":3,"lang":"eng","title":"Original (Eng)","codec":"ac3"}],
 "subtitles":[],
 "fast":{"sources":["audio"],"modes":["srt"],"min_chunk_minutes":1}}
{"type":"stage","t":"…","name":"extract"}
{"type":"stage","t":"…","name":"transcribe","chunk":1,"of":14,"from":"0:00:00","to":"0:10:00"}
{"type":"progress","t":"…","pct":42}
{"type":"language","t":"…","detected":"en"}
{"type":"stage","t":"…","name":"translate","chunk":1,"of":14}
{"type":"progress","t":"…","pct":87}
{"type":"saved","t":"…","path":"/…/movie.es.srt","covers":"0:10:00","watchable":true}
{"type":"warning","t":"…","kind":"blocks_untranslated","count":12,"total":1834,"first":[204,205,206]}
{"type":"done","t":"…","path":"/…/movie.es.srt","covers":"2:19:08"}
```

That is one producible run, trimmed to its first chunk: a run ends in **exactly one** of `done` or `error`, never both, and a `warning` always precedes the ending. An `error` event replaces the `done`:

```json
{"type":"error","t":"…","kind":"invalid_key","message":"…"}
```

`progress` carries a percentage for the current stage; it has no stage tag of its own, so a consumer reads it against the most recent `stage`. `saved` fires after every chunk in `-fast` mode, with `watchable` true from the first one.

The five stage names are the five phases of a run: `extract` (audio to WAV), `extract-subs` (an existing subtitle track out of the container, and parsed), `transcribe`, `translate`, `mux` (writing the new container). The GUI has a label for each. `extract` and `mux` now report progress like every other stage — see "Revised after implementation" below — so nothing here is a special case for the window to know about.

Exit codes, and the terminal event each pairs with:

| Exit | Meaning | Terminal event |
|---|---|---|
| 0 | the run finished | one `done` — none for `-probe`, `-version`, `-h` or a subcommand |
| 1 | a fatal error | one `error` with a `kind` — except an unsupported `-events` value, which cannot be answered in a format it does not understand |
| 2 | a usage error: an unknown flag or a bad flag value | one `error`, kind `other`, after `version` |
| 130 | cancelled by SIGINT or SIGTERM | one `error`, kind `cancelled` |

The GUI routes on the `kind` and falls back to the exit code, never the other way round.

### `-probe`

Prints the `version` and `probe` events and exits without doing any work. The GUI needs the track list before the user can choose anything, and today probing is welded to the start of a run.

With `-events json` it emits JSON; without it, the human-readable track listing the tool already prints. It requires only the input path — no `-to`, no key.

`probe.fast` states the `-fast` rule as data — the `-source` and `-mode` values it accepts, and the minimum `-chunk` in minutes — so the window can disable its "fast mode" checkbox with a reason instead of holding a second copy of the rule. It is on every `probe` event, `-probe`'s and the ordinary run's alike, since both build it through the same `probeEvent` function.

**`-probe`, `-version`, `-h` and the subcommands carry no terminal event.** None of them is a run: each emits `version` (and `-probe` its `probe`), exits 0, and emits no `done` and no `error`. A decoder shared between the probe call, a settings read and the run call must not wait for one.

`-events json` composes with the `config` and `model` subcommands rather than being refused by them, so a wrapper can append the flag to every invocation uniformly: the flag is taken out of the arguments the subcommand itself counts, the subcommand's own plain-text output on stdout is unchanged, and the stream carries the `version` event alone. The Settings screen still reads and writes through them as text.

### Typed errors

Every fatal error gains a `kind` so a caller can act on it rather than match on wording. The message stays exactly as it is today.

| `kind` | Raised when |
|---|---|
| `missing_key` | no OpenAI key in the environment or the config |
| `invalid_key` | the endpoint rejected the key: HTTP 401 or 403 |
| `whisper_not_found` | `whisper-cli` is neither configured nor on `PATH` |
| `no_model` | no ggml model configured or discovered |
| `ambiguous_model` | several models found, none chosen |
| `no_audio_track` | the file has no audio to transcribe |
| `no_subtitle_track` | `-source sub` on a file with none |
| `probe_failed` | ffprobe could not read the file |
| `output_unwritable` | a path the user chose — the sidecar `.srt` or the muxed container — could not be written |
| `rate_limited` | the endpoint returned HTTP 429 for every block |
| `transcript_empty` | the audio produced no speech |
| `cancelled` | SIGINT or SIGTERM |
| `other` | anything not yet classified — a usage error, an unreachable endpoint, a mangled reply |

The first five are what the GUI routes to its settings screen. `invalid_key` is separate from `rate_limited` because the two lead to different places: a key with a typo in it is corrected in Settings and retrying it can never succeed, while a rate limit is waited out on the Run screen. Folding them together sent the most likely first-run error to a Retry button that re-ran the whole transcription.

`output_unwritable` is not a settings problem: the GUI is what offers the save path in the first place, so it stays on the Run screen with the message and a Retry button, the same as `rate_limited` — the fix is to leave the message on screen while the person picks a different location and presses Retry, not to jump to a form field that had nothing wrong with it.

### Revised after implementation

Part 1 above is the design as built. What the build changed, and why:

- **The skip rule and the captured ffmpeg stderr.** "Newline-delimited JSON on stderr" was written without reckoning with what already writes there. Four ffmpeg invocations inherited the stream, and because ffmpeg's progress lines end in a carriage return with no newline, an event emitted during one was unparseable — which destroyed the `cancelled` event on the path most likely to produce it. The decision stands (stderr, not a dedicated fd) but now comes with two obligations: skip the non-JSON lines, and let no foreign writer near the stream.
- **`invalid_key`, and `rate_limited` narrowed to 429.** The original table had no kind for a rejected key, so a typo was reported as a rate limit and routed to a Retry button. Anything that is neither refusal — an unreachable endpoint, a reply the model mangled — is now `other`, which is honest: it is not something the GUI can route on.
- **Five stage names, two of them new.** `extract-subs` and `mux` were invisible phases: the `-source sub` flow went `probe` → `stage translate` with nothing in between, and a container rewrite left the window reading "translate, 100%" for minutes.
- **The exit-code table, including 2.** A usage error exits 2, which appeared nowhere in this document, and used to exit from inside the flag package before the emitter existed — so the one case version negotiation is for produced no `version` event. It now emits `version`, then `error`, then exits 2.
- **The sample stream.** The original chained `warning → done → error`, which no run can produce. Replaced with one producible run plus the `error` alternative, because a decoder test written from the old sample would have encoded an impossible sequence as a fixture.
- **An event never starts mid-line, and a lock alone did not achieve that.** The first attempt gave the events and the prose one mutex, which makes each write atomic; a SIGINT at an interactive prompt still glued the `cancelled` event onto the prompt's unfinished line, losing the same terminal event on the same Cancel path as the ffmpeg problem. The writer now tracks whether the last byte was a newline — a carriage return does not count — and the emitter breaks the line under the same lock.
- **The subcommands compose with the flag.** Refusing `-events json` on `config`/`model` was tried and reversed: the flag was ignored (so effectively worked) on `config path` and `config list` before the event stream existed, and refusing it shipped a regression on an invocation a script may already hold. They join `-probe` and `-version` as invocations that emit `version` and no terminal event.
- **`-events json` is honoured exactly where `flag.Parse` honours it.** The early argv scan that lets a parse failure be reported on the stream reads argv by the same rules the flag package is about to — it stops at the first positional argument — so `movie.mkv -events json` is ignored by both rather than by only one of them. It keeps looking past a flag the CLI does not define, because that is the case it exists for.
- **`OPENAI_BASE_URL` is now honoured, not just accepted by the internal `Options.BaseURL` a test could reach.** It was a plain environment read away from being reachable from outside the package, and until it was read in `resolveTranslateOptions` no test could drive the *compiled binary* through a successful translation — the ordinary run's event stream had none for exactly that reason. It is also independently useful for an OpenAI-compatible local server, a proxy, or Azure.
- **Extraction and muxing report progress too, now.** Capturing ffmpeg's stderr (the first bullet above) fixed the unparseable-event problem but took the progress counter with it — forty seconds of silence on a feature film's audio extraction, in the terminal and on the stream alike. `runFFmpeg` already scanned every stderr line to build the diagnostics tail; it now also parses the `time=` field off the progress lines it was dropping and reports it before discarding them, rather than adding a second pipe with `-progress`. `ExtractAudio` and `MuxSubtitle` report against `ContainerDuration`; `SliceWAV`'s chunk already knows its own length and needs no extra probe. Both the printed line and the `progress` event are computed from the same percentage, so they cannot disagree the way `pctOf` and the printed translate line once did. `ExtractSubtitle` was tried with the same parameter, for signature consistency with its three siblings, and the parameter came back out: wiring it non-nil triggered a `ContainerDuration` probe on every ordinary run whether or not anything was listening — exactly the unconditional-subprocess cost the probe-event work above was careful to avoid — and broke `TestOrdinaryRunSpawnsFfprobeOnlyWhenEventsAreOn`. Pulling one subtitle track out of a container is a text demux, not a minutes-long decode, so `ExtractSubtitle` takes no progress callback at all rather than one call site's `nil` inviting a future caller to assume it works.
- **The children go with the cancel.** SIGINT reaches the CLI alone — ffmpeg and whisper-cli are reparented to PID 1 — and capturing their stderr only collects the ones that write again: an orphan killed by EPIPE is an orphan that has to write, and a transcription loading a 1.6 GB model writes nothing for seconds. Each child is now started in a process group of its own (`internal/proc`, two build-tagged files, no new dependency, so `make build-all` keeps producing all five platforms), and the signal handler kills those groups after the `cancelled` event is emitted and before the scratch directory is removed — in that order, so nothing is still writing into a directory being deleted underneath it. Killing the children also unblocks the main goroutine as an ordinary `signal: killed` failure, which would race the handler to the exit: a second `error` event on a stream that promises one, and exit 1 instead of 130. So the handler raises a flag before the kill, and both other endings a run can reach — `exitWith`, and the one place a `done` event is written — wait for the handler instead of racing it, bounded so a wedged handler cannot hang the process forever. Reading the flag before `done` closes a hole that predates all of this: a cancel arriving in a run's last milliseconds used to produce exit 0 with both a `cancelled` and a `done` event on the stream. The flag is what makes the new handler-driven kill safe; what retired the *old* terminal exposure is the detach itself, because a foreground-group Ctrl-C used to kill ffmpeg directly and the resulting failure raced a handler that had nothing to do with it — those children are no longer in the foreground group. The three `ffprobe` calls are deliberately left in it: bounded metadata reads, reaped inside `.Output()`, worth no machinery. Windows gets a new console process group but has no group kill, so its `KillGroup` ends the child alone; that path is compile-verified only.

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

Fast mode is disabled unless the source is an audio track and the output is a sidecar `.srt`, with a line saying why rather than a dead control. The window reads that condition off the `probe` event's `fast` object rather than holding its own copy of the rule.

**Run.** The current stage in words, a progress bar, and for `-fast` the growing coverage with a prominent note the moment the file is watchable. A Cancel button. On completion, the path, a "Show in Finder" button, and any warning about untranslated blocks — carried through from the `warning` event, because a file that is 12 blocks short of translated must not look identical to one that is not.

**Settings.** OpenAI key (masked, with a Show toggle), model, workers, whisper binary, whisper model, VAD model, chunk length. Every one of these reads and writes the same `~/.config/sub-translator/config.json` through `sub-translator config get` and `config set`, so the GUI holds no second copy of the validation rules and a value set in either interface is seen by the other.

The path to the CLI itself sits apart, above a divider, labelled as the one setting the window keeps for itself — for the bootstrap reason given above.

### Visual design

Minimal, flat, and translucent — the window borrows the desktop behind it rather than painting its own background.

**The glass is real, not drawn.** Wails exposes the operating system's own compositor: `WindowIsTranslucent` with `WebviewIsTransparent` (the documentation calls this pair the way to make "frosty-looking applications"), `mac.TitleBar{TitlebarAppearsTransparent: true, FullSizeContent: true}` for a seamless top edge, and `BackdropType: windows.Mica` on Windows 11. The page itself has no background colour; the blur comes from AppKit and DWM. A CSS `backdrop-filter` over an opaque page is the imitation of this and looks like one.

**Legibility is the real cost of glass**, and it has to be paid explicitly. A user's wallpaper can be a white beach or a black-and-yellow poster, and text floating directly on the blur will fail against one of them. So content sits on a **scrim**: a single translucent layer, `rgba(255,255,255,0.72)` in light appearance and `rgba(28,28,30,0.72)` in dark, which keeps body text above a 7:1 contrast ratio over any wallpaper while still letting colour through at the edges. The blur stays visible in the window's margins and behind the title bar, which is where it reads as glass; it does not sit behind paragraphs.

**Flat means no decoration doing a job that layout can do.** No drop shadows except the one macOS gives the window itself, no gradients, no borders where whitespace separates just as well. Where a line is genuinely needed — between list rows, above the button bar — it is a one-pixel hairline at 8% of the foreground colour. Corner radius is 10px on cards and 6px on controls, matching the platform rather than inventing a house style.

**Type carries the hierarchy.** The system stack — `-apple-system` on macOS, `Segoe UI Variable` on Windows — so the window looks native and nothing is downloaded. Three sizes only: 13px for body and controls, 11px for secondary labels and hints, 20px semibold for the film's title. Numbers that change during a run (percentages, timecodes) use `font-variant-numeric: tabular-nums` so the layout does not jitter as they tick.

**One accent colour**, taken from the OS where it is available (`AccentColor` / `-apple-system-blue`) so it matches whatever the user chose in their system settings. It appears on exactly three things: the selected radio, the progress fill, and the primary button. Nothing else is coloured except errors, which are the system red, and the "you can start watching" line, which is the system green.

**Dark and light follow the OS**, through `prefers-color-scheme` and `mac.Appearance` — no in-app theme switch. Everything is expressed as CSS custom properties on `:root` and redefined once under the dark media query, so the two themes cannot drift apart.

```
╭──────────────────────────────────────────────╮
│                                              │   ← translucent titlebar,
│   Бойцовский клуб.mkv                        │     blur shows through
│   2:19:08 · MKV                              │
│                                              │
│  ╭────────────────────────────────────────╮  │   ← scrim card, 10px radius
│  │  Дорожка                               │  │
│  │  ◯  #1  rus   MVO Позитив-Мультимедиа  │  │
│  │  ◯  #2  rus   MVO РТР / РЕН-ТВ         │  │
│  │  ◉  #3  eng   Original                 │  │   ← accent on the selection
│  │  ────────────────────────────────────  │  │   ← 8% hairline
│  │  Субтитров в файле нет                 │  │
│  ╰────────────────────────────────────────╯  │
│                                              │
│  Язык  [ Испанский  ▾ ]   Вывод [ .srt  ▾ ]  │
│  ☑ Быстрый режим — смотреть через минуту     │
│                                              │
│                        [ Начать ]            │   ← accent, the only filled button
╰──────────────────────────────────────────────╯
```

The run screen replaces the card with a stage line, a progress bar and the growing coverage; the settings screen replaces it with form rows on the same scrim. One card, three contents — the window never changes shape or size between screens, which is what keeps it feeling like one surface rather than a wizard.

### Errors lead somewhere

An `error` event with `kind` of `missing_key`, `invalid_key`, `whisper_not_found`, `no_model` or `ambiguous_model` switches to Settings, focuses the field at fault, and shows the message above it. Every other kind stays on the Run screen with the message and a Retry button.

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
- The glass comes from the OS compositor, never from a CSS filter over an opaque page.
- Body text keeps a 7:1 contrast ratio over any wallpaper, which is what the scrim is for.
- No web fonts, no icon fonts, no CSS framework — the system stack and hand-written CSS.
- Go 1.26 floor. Conventional commits.
