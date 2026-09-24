# Whisper Transcription Design

**Date:** 2026-09-23
**Status:** Approved

## Goal

Let `sub-translator` produce translated subtitles for a video that has no subtitle track, by transcribing its audio with whisper.cpp. The transcript enters the existing pipeline as SRT, so everything downstream — translation, sidecar output, muxing — is unchanged.

A second goal is configuration: the path to the whisper binary and the ggml model must be settable once and remembered, not repeated on every invocation.

## Non-Goals

- Building or installing whisper.cpp. The user installs it (`brew install whisper.cpp`); we locate it and fail with an instruction when it is missing.
- Whisper's own `--translate` task. It only targets English, and the existing translator already handles every language pair.
- Speaker diarization, karaoke output, word-level timestamps.
- Migrating existing flags or the config format. This is the first config file the tool has ever written.

## Context

The tool today runs `probe → extract → parse → translate → write/mux` and aborts at `main.go:88` when the input has no subtitle track. `internal/media` already shells out to ffmpeg/ffprobe, `internal/srt` already parses and writes SRT, `internal/translate` already batches translation. The gap is only the *source* of the SRT.

`go.mod` has zero requires and `make build-all` cross-compiles to five platforms. Both facts constrain the design.

## Approach

Run the external `whisper-cli` binary via `exec`, exactly as ffmpeg is run today, and ask it for SRT output.

Rejected alternatives:

- **cgo bindings** (`whisper.cpp/bindings/go`): in-process progress callbacks, but breaks `make build-all` entirely, requires a C toolchain to build from source, and adds the project's first third-party dependency. The cost is not proportional to the benefit.
- **JSON-only output** (`-oj`, build blocks ourselves): needs a bespoke parser against an unversioned schema, while `srt.Parse` already exists and is tested.

The chosen approach asks for `-osrt` *and* `-oj` in one run: the SRT feeds `srt.Parse`, and the JSON's `result.language` field supplies the detected source language. Both are structured; neither requires scraping stderr.

## Whisper.cpp Facts

Verified against whisper.cpp master (`examples/cli/cli.cpp`) and Homebrew formula `whisper.cpp` 1.9.4 on 2026-09-23.

| Item | Value |
|---|---|
| Homebrew formula | `whisper.cpp` (old name: `whisper-cpp`) |
| Binary | `whisper-cli` |
| Model flag | `-m FNAME` |
| Audio input flag | `-f FNAME` |
| Output SRT | `-osrt` |
| Output JSON | `-oj` |
| Output base path (no extension) | `-of FNAME` |
| Language | `-l LANG`, `auto` to detect |
| Threads | `-t N` |
| Progress to stderr | `-pp` |
| VAD | `--vad`, `-vm FNAME`, `-vt N` |

`whisper-cli` reads WAV through its bundled decoder; other containers work only in builds compiled with ffmpeg support. The audio is therefore always transcoded first to 16 kHz mono signed-16-bit PCM WAV, which is also whisper's native input format — no quality is lost by doing so.

The JSON document contains `result.language`, the ISO 639-1 code whisper detected.

## Model Compatibility

The user runs Slaive (`net.slaive.app`), a whisper.cpp wrapper, which keeps models in
`~/Library/Application Support/net.slaive.app/models`:

| File | Size | Identity |
|---|---|---|
| `ggml-large-v3-turbo.bin` | 1 624 555 275 B | Whisper large-v3-turbo, ggml magic, `n_vocab` 51866 (multilingual) |
| `ggml-silero-v5.1.2.bin` | 885 098 B | Silero VAD v5.1.2 |

Both sizes match the upstream HuggingFace artifacts byte for byte, so the files are already in the format this design needs. Compatibility costs nothing: we adopt the same `ggml-<name>.bin` naming and read that directory.

**Resolution order** for the model, first hit wins:

1. `-whisper-model` flag
2. `whisper.model` in config
3. discovery across the search directories

The **search directories**, in order, are:

1. `models.dir` from config, when set
2. `~/Library/Application Support/sub-translator/models` (ours, the default `model pull` target)
3. `~/Library/Application Support/net.slaive.app/models` (Slaive's)
4. `~/.cache/whisper.cpp`
5. `./models`

On Linux, entry 2 becomes `$XDG_DATA_HOME/sub-translator/models` (falling back to `~/.local/share/...`) and entry 3 is skipped, Slaive being macOS-only. Directories that do not exist are skipped silently.

We **read** Slaive's directory but never **write** to it. `model pull` always writes to our own directory. Discovery treats any `ggml-*.bin` as a candidate transcription model except files whose name contains `silero` or `vad`, which are candidate VAD models instead.

The practical effect on the author's machine is zero-configuration operation: the large-v3-turbo model and the Silero VAD model are both found on first run.

## Components

Three new packages, each with one responsibility and no knowledge of the others.

### `internal/config`

Reads and writes `~/.config/sub-translator/config.json` (honouring `XDG_CONFIG_HOME`; the same path on every OS, chosen for predictability over `os.UserConfigDir()`, which differs per platform).

```go
type Config struct { ... }                    // typed fields, JSON tags
func Load() (*Config, error)                  // missing file is not an error
func (c *Config) Save() error                 // 0700 dir, 0600 file
func (c *Config) Get(key string) (string, error)
func (c *Config) Set(key, value string) error // validates key and value
func Keys() []string                          // for `config list` and errors
func Path() string
```

Keys are flat dotted strings: `whisper.bin`, `whisper.model`, `whisper.vad-model`, `whisper.vad-threshold`, `whisper.threads`, `whisper.language`, `models.dir`. An unknown key is an error naming the valid ones. Paths are stored expanded (`~` resolved at set time) so the file is unambiguous.

### `internal/models`

A static catalog plus a downloader.

```go
type Model struct { Name, URL, Filename string; Size int64; Kind Kind }  // Kind: Transcribe | VAD
func Catalog() []Model
func Find(name string) (Model, bool)
func SearchDirs(configuredDir string) []string   // the ordered search-directory list
func Discover(dirs []string) (models, vads []string)
func Pull(m Model, destDir string, progress func(done, total int64)) error
```

Download writes to `<dest>.part` and renames on success, so an interrupted pull never leaves a corrupt file that discovery would later pick up. Size is verified against `Content-Length` before the rename. Resume is out of scope; a failed pull restarts.

Catalog entries point at `https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-<name>.bin`, except the VAD model at `https://huggingface.co/ggml-org/whisper-vad/resolve/main/ggml-silero-v5.1.2.bin`.

### `internal/whisper`

```go
type Options struct {
    Bin, Model, VADModel, Audio, OutBase, Language string
    VADThreshold float64
    Threads int
}
func FindBinary(configured string) (string, error)  // config → whisper-cli → whisper-cpp in $PATH
func Args(o Options) []string                       // pure; the unit-test seam
func Run(o Options, progress func(pct int)) (lang string, srtPath string, err error)
```

`Args` is pure and carries the flag knowledge, so the whole argument contract is testable without the binary present. `Run` executes, streams stderr through a progress parser that recognises whisper's `progress = NN%` lines, and reads `<OutBase>.json` for the detected language.

VAD flags are emitted only when `VADModel` is non-empty. The author's setup supplies one, which matters on feature-length input: whisper is prone to repetition loops over long silences, and VAD removes them from the input.

### `internal/media` additions

```go
func AudioStreams(streams []Stream) []Stream
func FindAudioByLang(streams []Stream, lang string) (Stream, bool)   // reuses normLang
func ExtractAudio(input string, streamIndex int, outWAV string) error
```

`ExtractAudio` runs `ffmpeg -y -i <input> -map 0:<idx> -vn -ac 1 -ar 16000 -c:a pcm_s16le <out>`.

### `main` package additions

- `source.go` — a `sourceMode` type (`sub`, `audio`, `auto`) mirroring the existing `outputMode` in `mode.go`: parsing and resolution as pure functions, unit-tested without I/O.
- `cmd_config.go`, `cmd_model.go` — subcommand handlers.
- `main.go` — dispatches subcommands before flag parsing, then branches on the resolved source.

## Data Flow

```
                    ┌─ sub   ─→ media.ExtractSubtitle ──┐
probe → resolve ────┤                                   ├─→ srt.Parse → translate → write/mux
                    └─ audio ─→ media.ExtractAudio      │
                                → whisper.Run ──────────┘
```

Everything right of `srt.Parse` is existing, unmodified code.

## CLI Contract

```
sub-translator [flags] <input>
sub-translator config <list|get|set|path> [key] [value]
sub-translator model  <list|pull> [name]
```

New flags:

| Flag | Default | Meaning |
|---|---|---|
| `-source` | `auto` | `sub`, `audio` or `auto` |
| `-atrack` | `-1` | audio stream index, `-1` = auto |
| `-whisper-model` | — | overrides `whisper.model` |
| `-whisper-bin` | — | overrides `whisper.bin` |
| `-vad-model` | — | overrides `whisper.vad-model` |

Flags always override config; config always overrides discovery.

**Resolution of `auto`:** subtitle tracks present → `sub`. None present → `audio`, after an interactive confirmation, because transcribing a feature film costs many minutes of full-load CPU and must not start by surprise. When stdin is not a terminal the confirmation cannot be answered, so `auto` fails with an error telling the user to pass `-source audio` explicitly.

`-source audio` skips the confirmation and ignores existing subtitle tracks.

`-source sub` on a file with no subtitle tracks keeps today's error message.

**Language:** in audio mode `-from` is optional. When omitted, whisper runs with `-l auto` and the detected code becomes the source language for translation; the "Source language code" prompt is not shown. When given, it is passed as `-l <code>`, which is both faster and more accurate than detection.

**Transcript retention:** the source-language transcript is written next to the video as `<video>.<lang>.srt` before translation begins. Transcription is the most expensive step in the pipeline by orders of magnitude; discarding its output would be indefensible.

## Error Handling

Every failure names the thing to do next:

| Condition | Message |
|---|---|
| No whisper binary | `whisper-cli not found — install it (brew install whisper.cpp) or set it: sub-translator config set whisper.bin <path>` |
| No model configured or discovered | `no whisper model found — download one (sub-translator model pull large-v3-turbo) or set it: sub-translator config set whisper.model <path>` |
| Several models discovered, none chosen | lists the candidates, asks for `-whisper-model` or `config set` |
| Model path set but missing | `whisper model not found: <path>` |
| No audio streams | `no audio tracks in <file> — nothing to transcribe` |
| `-atrack N` not an audio stream | `no audio stream at index N` |
| whisper exits non-zero | its stderr tail, prefixed `whisper:` |
| Unknown config key | the key plus the list of valid keys |
| Unknown model name | the name plus `sub-translator model list` |

Temporary WAV and whisper output files go under `os.MkdirTemp` and are removed on exit, including on error. A 2-hour film yields roughly 1.2 GB of WAV, which is too much to leave behind.

## Testing

Pure functions carry the logic, so most of it is testable without whisper installed:

- `whisper.Args` — flag ordering, VAD flags present only with a VAD model, language default, threads omitted when zero
- `whisper.FindBinary` — config first, then each PATH name, error when absent
- `config` — round-trip save/load, get/set per key, unknown key, `~` expansion, file permissions
- `models` — name → URL and filename, catalog completeness, `Discover` classifying `silero`/`vad` files separately from transcription models, search-dir ordering
- `media.ExtractAudio` argument construction, `FindAudioByLang`, `AudioStreams`
- `source.go` — parsing, and `auto` resolution against "has subtitles" and "is a terminal"
- subcommand dispatch and argument validation

An end-to-end test that runs the real binary is gated on its presence and skipped otherwise, so CI stays green on machines without whisper.cpp.

**Manual acceptance** (the criterion the author set), run on
`~/Downloads/Бойцовский клуб (1999) WEBRip-AVC [Open Matte]/…mkv`:

1. `brew install whisper.cpp`
2. `sub-translator -source audio -to es <film>` finds the Slaive model with no configuration
3. A Spanish `.srt` is produced from the audio track, with plausible timings and text

Because transcribing a 139-minute film takes a long time, the acceptance run uses a short slice of the film cut with ffmpeg first, and the full run is the author's to make.

## Constraints

- Go 1.26 floor; no toolchain directive.
- `go.mod` must still have zero requires when this is done.
- `make validate` (gofmt, vet, test) must pass.
- No changes to `internal/srt` or `internal/translate`.
- Conventional commit messages, matching the existing history.
