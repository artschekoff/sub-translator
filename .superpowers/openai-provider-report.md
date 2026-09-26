# OpenAI translation provider — implementation report

Branch `feat/openai-translator`, base `4dd023c`. Six commits, `make validate` green,
`go test -race ./...` green, `go.mod` still has no requires.

## Commits

| SHA | Subject |
|---|---|
| `20e4e33` | refactor(translate): translate batches through a shared worker pool |
| `e228ca9` | feat(translate): add an OpenAI translation provider |
| `1e44d7d` | feat(config): remember the translation provider, key, model and workers |
| `d5e3961` | feat(cli): choose the translation provider with -provider and -workers |
| `7ac9007` | docs: document choosing a translation provider |
| `5a64fdb` | fix(config): do not echo a freshly set API key back in full |

## What was built

### `internal/translate/translator.go` (new)

`Translator`, `Options`, `Options.Validate`, `NewTranslator` exactly as specified, plus
the shared machinery both providers use:

- an internal `provider` interface — `batch(texts, before)` and `one(text, before)`,
  both with retries inside — so the pool is blind to which endpoint is behind it;
- `translateAll(p, texts, workers, progress)`, the shared body of both
  `TranslateAll` methods;
- `runBatch`, which writes into `results[start:]` only, never appends, and keeps the
  rule that a batch failing with a `statusError` does **not** fan out per block;
- `statusError` now carries `retryAfter`, and the single `retry` helper honours it
  over the widening backoff;
- `parseRetryAfter`, delta-seconds only. The HTTP-date form is ignored rather than
  guessed at (falling back to the backoff is never wrong, only slower), and the wait
  is capped at two minutes so a misconfigured proxy cannot hang a film for a day.

The five listed hazards are handled and each has a test:

1. `failed` is `slices.Sort`ed before it is returned.
2. The progress callback runs inside the mutex, accumulates blocks *translated*, and
   is clamped to the total; it can neither interleave, regress nor overshoot.
3. Results are written by index from each worker's own disjoint range.
4. `retryBaseDelay`, `perBlockSleep` and `batchSleep` are untouched package vars.
   `retryAfterUnit` was added on the same principle, so honouring a "wait 3 seconds"
   header costs 60 ms in the suite rather than three seconds.
5. An error comes back only when every block failed.

### `internal/translate/openai.go` (new)

`POST <baseURL>` (default `https://api.openai.com/v1/chat/completions`), `Bearer` auth,
`batchSize` (40) blocks per request. The user message is

```json
{"context_previous_lines_do_not_translate":["…","…","…"],"translate":{"0":"…","1":"…"}}
```

— the batch keyed by its index as a string, and the previous three source lines in a
field whose own name says not to translate them; the system prompt repeats the
instruction and asks for a JSON object with exactly those keys and nothing else.

No `response_format` is sent, per the brief. `decodeObject` takes the outermost
`{ … }` out of the content, so a fenced or prefaced object still parses. A key set
that is not the requested one — missing key, extra key, unparseable body — fails the
batch without a retry and falls through to the per-block path, exactly as a mangled
separator does on the Google side. `one` is the same request with a single key.

Timeout is 120 s (the Google client's 30 s is too tight for forty lines through a
chat model, and a timeout fails a whole batch).

### `internal/config`

New keys `translate.provider`, `translate.api-key`, `translate.model`,
`translate.workers`, all validated on `Set` with messages that name what is
acceptable. `Display` is `Get` with secrets masked (`sk-…a4f2`, first three and last
four runes) and is what `config list` prints; `config get` still returns the key in
full. `TestKeysAreSortedAndComplete` and the round-trip case list the new keys.

### CLI

`-provider` and `-workers` (sentinel `-1`, mirroring `-chunk`, so a typed `0` is
rejected by name instead of silently becoming the default), both in the `usage`
constant and the README flag list. `resolveTranslateOptions` is flag → config →
default, with `OPENAI_API_KEY` ahead of `translate.api-key`. The default provider
stays `google`.

The options are validated immediately after the input file is checked, i.e. before
the probe, the whisper lookup and the "this can take a while" prompt, so a missing
key cannot be discovered an hour into a transcription.

`newTranslateClient` is preserved as the swappable seam, now
`func(translate.Options) (translate.Translator, error)`. `progressive.go` can no
longer poke `client.From` once whisper reports the language (the interface has only
`TranslateAll`), so it rebuilds the client from the same validated options instead —
which cannot newly fail, and keeps the old client if it somehow did.

## Verification

```
$ /tmp/st -h
Flags:
  …
  -chunk         chunk length in minutes for -fast (default 10, or whisper.chunk-minutes)
  -provider      translation provider: google or openai (default: google)
  -workers       batches translated at once (default 4, or translate.workers)
  -whisper-model path to a whisper ggml model
  …

$ /tmp/st config list
  models.dir             (unset)
  translate.api-key      (unset)
  translate.model        (unset)
  translate.provider     (unset)
  translate.workers      (unset)
  whisper.bin            (unset)
  whisper.chunk-minutes  (unset)
  whisper.language       (unset)
  whisper.model          (unset)
  whisper.threads        (unset)
  whisper.vad-model      (unset)
  whisper.vad-threshold  (unset)

$ /tmp/st config set translate.provider openai
translate.provider = openai

$ /tmp/st config set translate.api-key sk-proj-notarealkey-a4f2
translate.api-key = sk-…a4f2

$ /tmp/st config list | grep translate
  translate.api-key      sk-…a4f2
  translate.model        (unset)
  translate.provider     openai
  translate.workers      (unset)

$ /tmp/st config get translate.api-key
sk-proj-notarealkey-a4f2

$ OPENAI_API_KEY= /tmp/st -to es movie.mkv      # provider openai, no key anywhere
error: the openai provider needs an API key: set OPENAI_API_KEY in the environment, or run `sub-translator config set translate.api-key <key>`
exit=1

$ /tmp/st -to es -workers 0 movie.mkv
error: -workers must be at least 1, got 0
exit=1

$ /tmp/st -provider deepl -to es movie.mkv
error: unknown translation provider "deepl"; valid providers: google, openai
exit=1
```

The `config set` / `config get` / failing-run commands were run against a scratch
`XDG_CONFIG_HOME` so the real `~/.config/sub-translator/config.json` was not
modified; `config list` above is the real one. No request reached
`api.openai.com` or `translate.googleapis.com` at any point — every test is on
`httptest`, and the failing runs stop at validation before any network call.

`make validate` (gofmt, vet, `go test ./... -v`): pass, 135 tests. `go test -race
./...`: pass.

## Deviations and judgement calls

1. **`newTranslateClient`'s signature changed** — it takes `translate.Options` and
   returns `(Translator, error)`. The variable, and the way the end-to-end tests swap
   it, are preserved; `stubTranslator` in `progressive_e2e_test.go` is a three-line
   edit. There was no way to reach a second provider, or to keep the interface at one
   method, without this.
2. **`runProgressive` takes one more parameter** (`topts translate.Options`, before
   `out`).
3. **`Options.Validate` was added** beyond the three names in the brief, so the
   missing-key error can be raised before the probe rather than after transcription.
4. **One observable change on the Google path**: the run now prints
   `Translating en → es via google...` (and `via openai gpt-6-luna, 4 workers...`)
   where it used to print `Translating en → es...`. With a paid provider in play this
   is the line that says what is about to be spent. Nothing else about the Google
   path changes at `-workers 1`: same request shape, same retries, same sleeps, same
   failure semantics, and every pre-existing test passes untouched.
5. **`config set translate.api-key` masks its echo too** (commit `5a64fdb`). The
   brief only specified `config list`; echoing a key back in full puts it in one more
   scrollback than it needs to be in, and `config get` remains the one command that
   prints it whole.
6. **`translate.New` keeps `Workers: 1`**, so the plain constructor is still the
   sequential client it always was; the 4-worker default is applied by
   `NewTranslator`, i.e. by the flag/config path.
7. **Batch keys are within-batch indices** (`"0"`…`"39"`), not absolute block
   numbers, because `provider.batch` deliberately does not know where its batch sits.
   Validation is key-set equality either way.
8. **`internal/config` hardcodes the strings `google` and `openai`** rather than
   importing `internal/translate` for the constants, to keep the config package
   dependency-free. There is a test in each package covering the pair.
