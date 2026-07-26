<div align="center">

<img src="assets/cover.png" alt="sub-translator" width="100%"/>

<br/>
<br/>

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat-square&logo=go&logoColor=white)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-blue?style=flat-square)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-macOS%20%7C%20Linux%20%7C%20Windows-lightgrey?style=flat-square)](https://github.com/artschekoff/sub-translator/releases)

**One command turns a movie you can't understand into a movie you can.**

Extracts the subtitle track from a video file, translates it, and muxes the new track back in — no API keys, no accounts, no re-encoding.

[The Problem](#the-problem) · [Installation](#installation) · [Usage](#usage) · [How It Works](#how-it-works) · [Development](#development)

</div>

---

## The Problem

You have a video with subtitles in a language you don't read. Getting it into a language you do read is normally a five-tool chore:

| The usual pain | What `sub-translator` does |
|---|---|
| **Manual extraction.** Open MKVToolNix, hunt for the right stream index, demux an `.srt` by hand. | Auto-detects the subtitle track by language code — `eng`, `en`, doesn't matter — and extracts it for you. |
| **Web translators mangle SRT files.** Paste a subtitle file into a translator and it "helpfully" translates the timecodes, breaks block numbering, and returns something no player will load. | Parses the SRT properly. Timing lines are copied **byte-for-byte**; only dialogue text is ever sent for translation. |
| **API keys and billing.** Most translation tooling wants a cloud account, a credit card, and a quota dashboard before it will translate one file. | Zero setup. Uses Google Translate's free public endpoint — no key, no login, no quota to manage. |
| **Slow, one-line-at-a-time translation.** Naive tools fire one request per subtitle block: thousands of round-trips for a feature film. | Batches ~40 blocks per request, with automatic per-block retry so one bad block can't poison the batch. |
| **Re-muxing loses quality.** Re-encoding to add a subtitle track costs an hour of CPU and a generation of video quality. | Stream-copies everything. Video and audio are untouched; only a new subtitle track is added. Takes seconds. |
| **AVI can't hold subtitles.** So the tool errors out and you're left with nothing. | Detects the container limitation and writes an external `.srt` next to the video instead. |

The result is one command, a few seconds of waiting, and a file that plays with translated subs in any player.

## Features

- **Automatic track detection** — finds the right subtitle track by language code, no manual stream index needed
- **Timing-safe translation** — timecode lines are never sent to the translator, so sync is preserved exactly
- **Smart batching** — groups subtitle blocks for fewer requests, with per-block fallback on failure
- **ISO 639-1/2 aware** — handles both `en` and `eng`, `es` and `spa` transparently
- **Multi-format muxing** — embeds the translated track into MKV/MP4; saves external SRT for AVI
- **Zero API keys** — no account, no credentials, no rate-limit dashboard
- **Lossless** — stream copy only; your video and audio bits are never re-encoded
- **Progress output** — live `%` counter so you know it's working

## Supported Formats

| Container | Embed subtitle track | External SRT |
|-----------|:-------------------:|:------------:|
| MKV       | ✅                  | optional (`-srt`) |
| MP4 / M4V / MOV | ✅            | optional (`-srt`) |
| AVI       | ❌ (not supported by container) | ✅ auto |

## Installation

**Prerequisite:** [ffmpeg](https://ffmpeg.org/download.html) must be in `$PATH` — it does the probing, extraction and muxing.

```bash
# macOS
brew install ffmpeg

# Ubuntu / Debian
sudo apt install ffmpeg
```

### From a release

Grab the archive for your platform from [Releases](https://github.com/artschekoff/sub-translator/releases), unpack it, and put the binary on your `$PATH`:

```bash
tar -xzf sub-translator-darwin-arm64.tar.gz
sudo mv sub-translator-darwin-arm64 /usr/local/bin/sub-translator
```

### From source

Needs [Go 1.26+](https://golang.org/dl/).

```bash
git clone https://github.com/artschekoff/sub-translator
cd sub-translator

make build      # -> ./bin/sub-translator
make install    # -> /usr/local/bin/sub-translator (uses sudo)
```

## Usage

```
sub-translator [flags] <input>
```

```
Flags:
  -from    source language code  (default: en)
  -to      target language code  (required — prompted for if omitted)
  -track   subtitle stream index, -1 = auto-detect (default: -1)
  -srt     also save translated .srt alongside the output file
  -no-mux  skip muxing, only save translated .srt
  -out     custom output file path
  -version print version and exit
```

### Examples

```bash
# Translate English subs to Spanish, embed into new MKV
sub-translator -to es movie.mkv

# Omit -to and you'll be asked for it
sub-translator movie.mkv
# Target language code (e.g. es, fr, de, ja): fr

# Save SRT only, skip muxing
sub-translator -to fr -no-mux movie.mkv

# Also keep the .srt file alongside the output
sub-translator -srt -to de movie.mp4

# Pick a specific subtitle track by stream index
sub-translator -to ru -track 3 movie.mkv

# Custom output path
sub-translator -to es -out /tmp/movie_es.mkv movie.mkv
```

`-to` has no default — translating into an arbitrary language silently is worse than asking, so an omitted `-to` prompts on stdin (and errors out when stdin isn't interactive).

If auto-detection can't find a track for `-from`, the tool prints every subtitle stream it *did* find — index, language and title — so you can pick one with `-track`. If the file has no subtitle streams at all, it exits with `nothing to translate`.

### Output naming

| Input | Flag | Output |
|-------|------|--------|
| `movie.mkv` | *(default)* | `movie.ES.mkv` |
| `movie.mp4` | `-to fr` | `movie.FR.mp4` |
| `movie.mkv` | `-srt` | `movie.ES.mkv` + `movie.es.srt` |
| `movie.avi` | *(any)* | `movie.es.srt` *(AVI only)* |

## Languages

Use standard [ISO 639-1](https://en.wikipedia.org/wiki/List_of_ISO_639-1_codes) two-letter codes. Both `en`/`eng` forms are accepted as input.

| Code | Language   | Code | Language   | Code | Language  |
|------|------------|------|------------|------|-----------|
| `es` | Spanish    | `fr` | French     | `de` | German    |
| `it` | Italian    | `pt` | Portuguese | `ru` | Russian   |
| `zh` | Chinese    | `ja` | Japanese   | `ko` | Korean    |
| `ar` | Arabic     | `pl` | Polish     | `nl` | Dutch     |
| `tr` | Turkish    | `uk` | Ukrainian  | `sv` | Swedish   |

Any language code supported by Google Translate works — the table above is just a reference.

## How It Works

```
input.mkv
    │
    ├─ ffprobe   →  detect subtitle tracks
    ├─ ffmpeg    →  extract .srt (stream copy, no decode)
    ├─ parse     →  split into blocks, preserve timing lines exactly
    ├─ translate →  batch requests to Google Translate (40 blocks/req)
    ├─ rebuild   →  merge translated text back, timing untouched
    └─ ffmpeg    →  mux new track into output container (stream copy)
```

Timing lines (`00:00:00,000 --> 00:00:00,000`) are copied verbatim — translation never touches them. Every ffmpeg call is a stream copy, so the pipeline is I/O-bound, not CPU-bound: a two-hour film is done in seconds plus translation time.

## Project Layout

```
main.go               flag parsing + pipeline orchestration
internal/media/       ffprobe / ffmpeg wrappers: probe, extract, mux, path naming
internal/srt/         SRT parse, rebuild, write
internal/translate/   batched Google Translate client
```

## Development

| Target | What it does |
|--------|--------------|
| `make build`     | Build `./bin/sub-translator` with the version stamped in |
| `make build-all` | Cross-compile darwin/linux (amd64 + arm64) and windows/amd64 |
| `make pack`      | `build-all`, then archive each binary into `./bin/dist` |
| `make install`   | Build and copy to `/usr/local/bin` (uses `sudo`) |
| `make test`      | `go test ./... -v` |
| `make fmt`       | `gofmt -w .` |
| `make vet`       | `go vet ./...` |
| `make tidy`      | `go mod tidy` |
| `make validate`  | `fmt` + `vet` + `test` |
| `make clean`     | Remove `./bin` |
| `make release`   | Interactive version bump → tag → push → pack → GitHub release |

`make release` reads the latest tag, prompts for a `major`/`minor`/`patch` bump (default `patch`), tags and pushes the new version, packs the cross-compiled archives, and publishes them with `gh release create --generate-notes`. It needs the [`gh` CLI](https://cli.github.com/) authenticated against this repo.

## License

MIT — see [LICENSE](LICENSE).
