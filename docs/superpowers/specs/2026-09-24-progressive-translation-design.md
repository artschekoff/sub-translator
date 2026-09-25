# Progressive Translation Design

**Date:** 2026-09-24
**Status:** Approved

## Goal

Let the user start watching a film a minute after asking for subtitles, instead of waiting for the whole transcription. The tool writes a usable `.srt` covering the opening of the film as soon as that part is done, then keeps extending the same file while the viewer watches, and says so when the file is complete.

A second, smaller goal travels with it: `internal/translate` currently reports a total failure as a complete success. That has to stop before a file starts growing in the background, because a silent failure in a later chunk would be invisible.

## Non-Goals

- Resuming an interrupted run from a partial file. A rerun starts over.
- Overlapping chunk boundaries to avoid splitting a sentence. Accepted limitation, stated below.
- Re-using a transcript across runs. Still not implemented; still documented as not implemented.
- Any GUI work. The flag added here is what a later GUI checkbox will drive.

## Context

Measured on this machine (Apple M1 Pro, Metal, `ggml-large-v3-turbo`):

| Fact | Value |
|---|---|
| Transcription speed | 14.3× real time at defaults; 18.2× with `-t 6 -p 2` |
| A 2 h 19 m film | ≈9 min 45 s of transcription |
| Audio extraction | 496× real time — ≈17 s for the same film |
| 16 kHz mono s16 WAV | 32 000 B/s → ≈267 MB for that film |

Transcription therefore outruns playback by more than an order of magnitude. The only reason a viewer waits at all is that the current pipeline produces nothing until the last block is done.

Two measured facts shape the design and are recorded so nobody re-litigates them:

- Disabling beam search (`-bs 1 -bo 1`) is **slower**, not faster: 17.9 s versus 12.9 s on the same audio, and it fragments the output from 28 blocks to 104. Whisper's temperature fallback re-runs segments when the beam is gone.
- The quantized `large-v3-turbo-q5_0` model is **not faster** on Metal (13.0 s versus 13.1 s) and is measurably less accurate. Quantization buys disk and memory here, not time.

Neither is used.

## Approach

Cut the work into fixed-length chunks, translate each as it lands, and rewrite the output file after every chunk.

Rejected alternatives:

- **Two chunks (first N minutes, then the remainder).** The remainder of a 139-minute film lands at ≈9 min 30 s while the viewer reaches minute 10 of the film at ≈10 min 40 s. A one-minute margin is not a margin.
- **Streaming whisper against a growing file.** whisper.cpp reads a complete WAV; feeding it a file still being written is not supported and would trade a simple design for a fragile one.

**One decode pass.** The full audio is extracted once, then sliced. Because 16 kHz mono s16 PCM is constant bitrate, a byte offset is an exact time offset, so slicing is both instant and sample-accurate. Decoding the 3 GB source once per chunk would cost more than the transcription it feeds.

**Uniform chunks.** Every chunk is the same length (default 10 minutes). The lead over the viewer grows monotonically: by the time they watch minute 10, the file covers minute 120.

**Language detected once.** The first chunk runs with `-l auto` when `-from` is absent; every later chunk is given the detected code explicitly. Per-chunk detection risks whisper changing its mind mid-film and producing a file in two languages.

**Chunk boundaries split sentences.** A line spoken across a boundary becomes two lines. This is accepted rather than solved: overlap-and-deduplicate adds a matching heuristic whose failures are harder to explain than the artifact it removes.

## Part 1: Honest translation failures

`internal/translate.TranslateAll` today keeps the original text for any block it cannot translate and returns a nil error, so a wholly failed translation prints `progress: 1834/1834 (100%)` and `Done.`

### Changes

```go
func (c *Client) TranslateAll(texts []string, progress func(done, total int)) ([]string, []int, error)
```

The second return value is the indices of blocks that kept their original text. The error is non-nil **only when every block failed**.

- `translateOne` gains an HTTP status check. A non-200 response becomes an error naming the status, with rate limiting (429) called out by name instead of surfacing as an unhelpful JSON parse failure.
- Retries with backoff wrap `translateOne`: four attempts at 1 s, 2 s and 4 s, for 429 and 5xx only. Other 4xx responses fail immediately, because repeating a malformed request is pointless.
- The existing batch-then-per-block cascade is kept. It is correct: a separator mangled by the translator is a batch problem, not a block problem. Only blocks that fail *after* the per-block retry count as failures.
- `apiURL` moves from a package constant to a `Client` field defaulting to the same value, so tests can point at `httptest`.

Retries help with brief throttling. They do not help with a sustained block — this host sat at HTTP 429 for over nineteen minutes during development. The value in that case is the report, not the retry.

`main.go` treats a non-nil error as fatal and a non-empty failure list as a warning naming the count and the first few block numbers, then writes the file anyway.

## Part 2: Progressive output

### `internal/media`

```go
func ExtractAudio(input string, streamIndex int, outWAV string) error          // unchanged
func SliceWAV(inWAV string, start, dur time.Duration, outWAV string) error
func WAVDuration(path string) (time.Duration, error)
```

`SliceWAV` runs `ffmpeg -y -i <in> -ss <start> -t <dur> -c copy <out>`. Stream copy on PCM is a byte-range operation.

### `internal/srt`

```go
func Shift(blocks []Block, by time.Duration) []Block
```

Chunk *n* is transcribed as if it began at zero, so its blocks are shifted by `n × chunkLen` before merging. Block numbers are renumbered on write, which `srt.Write` already does.

### `main`

A new file `progressive.go` owns the loop, keeping `main.go` orchestration-only:

```go
type chunkPlan struct {
    Index  int
    Start  time.Duration
    Dur    time.Duration
}
func planChunks(total, chunkLen time.Duration) []chunkPlan
func runProgressive(...) error
```

`planChunks` is pure and carries the arithmetic — the last chunk is short, a total shorter than one chunk yields exactly one chunk, and a zero total yields none.

The loop, per chunk: slice → whisper → shift → append to the accumulated blocks → translate only the new blocks → rewrite the whole `.srt` from everything accumulated so far.

Rewriting the entire file each time, rather than appending text, keeps block numbering correct and costs nothing at these sizes.

### Output and messaging

The file is the same `<video>.<lang>.srt` the normal mode writes, at the same path. After the first chunk the tool prints that the file is ready to watch with how much of the film it covers; after each later chunk it prints the new coverage; at the end it prints completion.

```
Saved SRT: movie.es.srt — covers 0:00–10:00, you can start watching
  extended to 20:00
  extended to 30:00
...
Done: movie.es.srt covers the full 2:19:08 — reload subtitles in your player
```

On macOS the completion line is accompanied by a desktop notification via `osascript -e 'display notification ...'`, which needs no dependency. Other platforms print only; a missing or failing `osascript` is ignored rather than failing the run.

The source-language transcript sidecar is written the same way, growing with each chunk. It uses the detected source language in its name (`movie.en.srt`) while the translation uses the target (`movie.es.srt`), so the two never collide unless source and target are the same language — which is already a no-op request.

### CLI

```
-fast            translate progressively so the opening is watchable within a minute
-chunk N         chunk length in minutes (default 10)
```

`-fast` is off by default: the plain mode stays exactly as it is today. `-chunk` is ignored without `-fast`. The config gains `whisper.chunk-minutes` as the default for `-chunk`.

`-fast` applies to transcription only. It is therefore valid with `-source audio`, and with `-source auto` **when auto resolves to audio**; the check runs after `resolveSource`, not before it. With `-source sub`, or with `auto` that found a subtitle track, there is nothing slow to hide — the track is already on disk — so `-fast` is an error naming why, rather than a silent no-op.

`-fast` with `-mode mux` or `-mode both` is also an error: a container cannot be rewritten incrementally, so the progressive promise cannot be kept. The message says to use the default `-mode srt`.

## Error Handling

| Condition | Behaviour |
|---|---|
| A chunk's whisper run fails | Fatal. The file keeps the chunks already written, and the error names which chunk failed and what the file currently covers. |
| A chunk's translation partly fails | Warning naming the count; the run continues. |
| A chunk's translation wholly fails | Fatal, same as above — the file keeps what it has. |
| `-fast` with a resolved source of `sub` | Error, raised after source resolution and before any transcription. |
| `-fast` with `-mode mux`/`both` | Error before any work. |
| `-chunk` below 1 or above the film's length | Below 1 is an error; above the length silently yields a single chunk. |

Partial output surviving a mid-run failure is deliberate: ten minutes of usable subtitles beats none, and the message says exactly how much is there.

The run-scoped temp directory already added for the signal handler holds the full WAV and every slice, so Ctrl-C cleans all of it up.

## Testing

Pure functions carry the logic:

- `planChunks` — exact multiples, a short tail, a total below one chunk, zero, and a chunk length larger than the total
- `srt.Shift` — offsets applied to both timestamps, zero offset is identity, ordering preserved
- `media.SliceWAV` argument construction
- `translate` — total failure returns an error; partial failure returns indices and no error; a server answering 429 twice then 200 succeeds with no failures; a 400 is not retried (assert the request count); the batch separator round-trips
- flag validation — `-fast` with `sub`, with `mux`, with `both`, and `-chunk 0`

An end-to-end progressive run against the stub whisper binary already used by `internal/whisper`'s tests: three chunks, asserting the output file exists and grows after each, that timestamps in later chunks are offset correctly, and that the final file covers the whole input.

Manual acceptance on a real film once one is available again: confirm the first chunk lands inside a minute, that the file is playable at that point, and that the completion notification fires.

## Constraints

- Go 1.26 floor; no toolchain directive.
- `go.mod` gains **no** third-party dependencies.
- `make validate` passes.
- The non-`-fast` path must behave exactly as it does today.
- Conventional commit messages.
