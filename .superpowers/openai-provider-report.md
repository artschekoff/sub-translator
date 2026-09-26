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

---

# Review round 2 — findings 1–10

Six further commits. `make validate`: pass, **171** test cases (was 135).
`go test -race ./...`: pass, no data race. Working tree clean.

| SHA | Subject |
|---|---|
| `f7759c7` | fix(translate): bound what one failing batch can cost — items 1, 3, 7 |
| `844fbf6` | test(translate): prove the failed indices come back sorted — item 2 |
| `4ea6d39` | fix(translate): default to one worker against Google's free endpoint — item 4 |
| `2818568` | fix(translate): retry an OpenAI batch once before fanning out — items 6, 8, 9 |
| `7307544` | fix(cli): name the provider in a -fast run too — items 5, 10 |

## 1 (HIGH) — the fan-out no longer outlives the endpoint

Reproduced first, exactly as described: a handler answering the batch `200` with an
unsplittable result and then refusing every block, `Retry-After: 1`, one 40-block
batch.

```
--- FAIL: TestTranslateAllStopsFanningOutWhenThrottlingStarts (120.43s)
    161 requests for one throttled batch, want at most 5
    requests: 161 (unbounded shape would be 161)
```

**161 requests, 120.43 s of wall clock, one batch, one worker.** After the fix, the
same handler:

```
--- PASS: TestTranslateAllStopsFanningOutWhenTheEndpointRefuses (0.13s)
    throttled  requests: 5 (unbounded shape would be 161)
    faulting   requests: 9 (unbounded shape would be 161)
```

New test: `TestTranslateAllStopsFanningOutWhenTheEndpointRefuses`, two subtests
(`throttled`, `faulting`), asserting the request budget, that every block keeps its
source text, and that `failed` is sorted.

**One refinement of your instruction, raised rather than taken silently.** "Stop the
moment `p.one` returns anything that `errors.As`-es to `statusError`" breaks
`TestTranslateAllPartialFailureReportsIndices` — the existing case titled *"One bad
block must not throw away the other 1833"*, where the endpoint 500s for one block's
content (`BOOM`) and translates its neighbours fine. Stopping on that first 500
marked blocks 1 **and** 2 failed: `failed = [1 2], want [1]`.

So the rule is split by what the status actually says:

- **429** — a statement about *us*, not about a block. The first one ends the batch;
  every further request makes the throttling worse.
- **5xx** — may genuinely be about one block's content. One is tolerated, a second
  ends the batch.

Both shapes stay `1 + O(1)` (5 and 9 requests above, against 161), and the existing
guarantee survives. If you would rather have the stricter rule, the existing test is
what has to change, and I would want you to say so rather than assume it.

## 2 (HIGH) — sorted indices now pinned

New test: `TestTranslateAllSortsFailedIndicesWhateverTheCompletionOrder`. Three
batches; the low-index one sleeps 40 ms before failing, so it completes last.
Verified by mutation — removing `slices.Sort(failed)` and its import:

```
--- FAIL: TestTranslateAllSortsFailedIndicesWhateverTheCompletionOrder (0.20s)
    failed = [40 41 42]… is not sorted
    failed[0] = 40, want 0 — the message would point at the wrong part of the film
```

## 3 (MEDIUM-HIGH) — `maxRetryAfter` 2 min → 30 s

Honoured for both providers still, capped at 30 s. Covered by the existing
`TestParseRetryAfterIgnoresWhatItCannotUse` case (`"86400"` → `maxRetryAfter`).

## 4 (MEDIUM) — one worker for Google, four for OpenAI

`DefaultWorkers` is gone; `DefaultWorkersFor(provider)` replaces it, with
`defaultGoogleWorkers = 1` and `defaultOpenAIWorkers = 4`, each carrying the reason
in a comment. `resolveWorkers` takes the provider, so `-workers` and
`translate.workers` still override in both directions. New/updated tests:
`TestDefaultWorkersForIsProviderSpecific`, `TestNewTranslatorChoosesTheProvider`
(asserts the Google client gets 1 and openai 4, and fails loudly if
`defaultGoogleWorkers` is ever raised), `TestResolveWorkers` (now a 7-row table over
both providers), `TestResolveTranslateOptions`. README gained a per-provider table
saying parallelism is for an API with a quota you pay for, and `-h` now reads
`default 1 for google, 4 for openai`.

## 5 (MEDIUM) — `-fast` names the provider

`runProgressive` prints `Translating via openai gpt-6-luna, 4 workers...` once,
before the first chunk. New test: `TestRunProgressiveNamesTheProvider`, with a
`captureStdout` helper that drains the pipe in a goroutine (a progressive run prints
far more than a pipe buffer holds).

## 6 (MEDIUM) — the cost model, corrected in code as well as comments

`malformedAttempts = 2`: a malformed reply is retried as a batch once — one request —
before the pool falls back to `len(batch)` requests that each re-send the ~280-token
system prompt and the context lines. A **status** failure is still never retried here,
so the item-1 bound is untouched. `mapReply` was extracted so the retry loop stays
readable. Both misleading comments rewritten. New test:
`TestOpenAIRetriesTheBatchBeforeFanningOut` (malformed once, then fine: exactly 2
requests, no block translated alone).
`TestOpenAIMalformedResponseFallsThroughToPerBlock` now expects
`malformedAttempts + len(texts)`.

## 7 (LOW-MEDIUM) — the provider's result length is no longer trusted

A batch whose reply is not exactly `len(batch)` strings fails the batch, and the copy
is bounded to `results[start:start+len(batch)]`. New test:
`TestTranslateAllRejectsAWrongLengthBatch`, driving `translateAll` with a
`countingProvider` that appends an `OVERFLOW` string to every batch, over two batches
under four workers: the per-block path rescues it and nothing from batch 0 lands in
batch 1's range.

## 8 (LOW) — `decodeObject` stops at the first complete value

`json.NewDecoder(strings.NewReader(s[start:])).Decode(&got)`, no search for a closing
brace. New `TestDecodeObjectToleratesWrapping` row:
`{"0":"uno"} (tell me if {anything} looks off)`.

## 9 (LOW) — renumbering pinned

Fourth case in `TestOpenAIMalformedResponseFallsThroughToPerBlock`:
`renumbered keys`, shifting every key by one. Same-size, wrong set — rejected, falls
through to per-block, all three blocks correct.

## 10 (optional) — `NewWithBaseURL` deleted

No callers remained. `Options.BaseURL` through `NewTranslator` is the only seam now,
and the comment in `openai.go` that referred to it was updated.

## Re-verified

```
$ /tmp/st -h | grep -E 'provider|workers'
  -provider      translation provider: google or openai (default: google)
  -workers       batches translated at once (default 1 for google, 4 for openai)

$ OPENAI_API_KEY= /tmp/st -to es movie.mkv      # translate.provider = openai
error: the openai provider needs an API key: set OPENAI_API_KEY in the environment, or run `sub-translator config set translate.api-key <key>`
exit=1
```

`make validate`: pass, 171 cases. `go test -race ./...`: pass. Left alone as agreed:
the worker-count upper bound, `Mask`'s threshold, the README's cost wording.

## One thing I could not do

`git add -A` in commit `f7759c7` swept `.superpowers/openai-provider-report.md` and
`.superpowers/review-openai.diff` (the review artifact, ~100 KB) into the branch.
They are working notes and do not belong in it. Taking them back out needs either a
history rewrite or `git rm --cached`, both of which this session is not permitted to
run, so the decision is yours:

```bash
git rm -r --cached .superpowers
printf '\n# Working notes from agent runs\n/.superpowers/\n' >> .gitignore
git commit -m "chore: stop tracking agent working notes"
```

That keeps both files on disk. No code, test or documentation commit depends on them.
