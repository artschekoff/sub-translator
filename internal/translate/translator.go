package translate

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Translator is the one thing the pipeline needs from a translation backend:
// turn every subtitle text into its translation, say which blocks it could not
// do, and report progress while it works.
//
// main.go and the progressive run are written against this rather than against
// the endpoint behind it, which is what lets the end-to-end tests drive a whole
// run without going near the network.
type Translator interface {
	TranslateAll(texts []string, progress func(done, total int)) ([]string, []int, error)
}

const (
	// DefaultModel is the OpenAI model subtitle translation defaults to. Of the
	// current line — Astra (flagship, complex reasoning), Sol (balanced), Luna
	// (cost-sensitive, high volume) — subtitles are the high-volume,
	// low-reasoning case, which is Luna.
	DefaultModel = "gpt-6-luna"

	// DefaultWorkers is how many batches are translated at once when neither
	// -workers nor translate.workers says otherwise. Four is enough to hide the
	// round-trip latency that otherwise dominates a run, against a paid endpoint
	// with real, documented rate limits.
	DefaultWorkers = 4

	// contextLines is how many already-seen source lines are handed to the model
	// as context. Three is enough to carry who is addressing whom, names and
	// formality across a batch boundary.
	contextLines = 3

	// batchSize is how many subtitle blocks travel in one request.
	batchSize = 40

	// maxAttempts is how many times one request is sent before a retryable
	// failure is treated as final.
	maxAttempts = 4
)

// Package-level vars allow tests to override timing without changing production
// behaviour.
var (
	retryBaseDelay = time.Second
	perBlockSleep  = 150 * time.Millisecond
	batchSleep     = 300 * time.Millisecond
)

// Options describes the translator a run wants. It is deliberately a plain
// struct: main.go resolves flags over config over defaults into one of these,
// and the end-to-end tests fill in BaseURL to keep a whole run off the network.
type Options struct {
	From, To string
	APIKey   string
	Model    string
	Workers  int
	BaseURL  string // test seam; empty means the real endpoint
}

// Validate reports the option errors worth raising before any work happens. It
// is exported separately from NewTranslator so a run can refuse a missing API
// key up front, rather than after an hour of transcription.
func (o Options) Validate() error {
	if strings.TrimSpace(o.APIKey) == "" {
		return errors.New("translation needs an OpenAI API key: " +
			"set OPENAI_API_KEY in the environment, or run " +
			"`sub-translator config set translate.api-key <key>`")
	}
	return nil
}

// NewTranslator builds the translator the options describe. An empty model or
// worker count takes the default, so a partially configured machine still runs.
func NewTranslator(o Options) (Translator, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if o.Workers < 1 {
		o.Workers = DefaultWorkers
	}
	return newOpenAI(o), nil
}

// provider is the backend as the worker pool sees it. Keeping the pool blind to
// the endpoint behind it is what lets the ordering, progress and
// failure-accounting rules be written once — and lets them be tested against a
// scripted provider no real endpoint would imitate.
type provider interface {
	// batch translates every text in one request, retries included. before holds
	// the source lines immediately preceding the batch, as context for the model.
	batch(texts, before []string) ([]string, error)
	// one translates a single text, for when a batch comes back unusable.
	one(text string, before []string) (string, error)
}

// translateAll is the body of the provider's TranslateAll: it splits the texts
// into batches, translates up to workers of them at a time, and accounts for
// what failed.
//
// Four rules are load-bearing here and each of them was a bug once:
//   - results are written by index, never appended, because the order of the
//     slice is the whole correctness of a subtitle file;
//   - failed comes back sorted, because the caller shows the first few to the
//     user and the pool finishes batches out of order;
//   - progress counts blocks translated, not blocks attempted, and is called
//     under a lock so it can neither interleave nor go backwards;
//   - an error is returned only when every block failed. A partial failure is
//     reported through the indices with a nil error.
func translateAll(p provider, texts []string, workers int, progress func(done, total int)) ([]string, []int, error) {
	total := len(texts)
	results := make([]string, total)
	if total == 0 {
		return results, nil, nil
	}
	if workers < 1 {
		workers = 1
	}

	var (
		mu      sync.Mutex
		failed  []int
		lastErr error
		done    int
	)

	// record folds one finished batch into the shared accounting. Everything the
	// workers share is touched here and nowhere else, and the progress callback
	// runs inside the lock: the caller's callback writes to a terminal and is
	// not itself safe to run from four goroutines at once.
	record := func(translated int, batchFailed []int, err error) {
		mu.Lock()
		defer mu.Unlock()
		failed = append(failed, batchFailed...)
		if err != nil {
			lastErr = err
		}
		done += translated
		if done > total {
			done = total
		}
		if progress != nil {
			progress(done, total)
		}
	}

	starts := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for start := range starts {
				end := min(start+batchSize, total)
				before := texts[max(0, start-contextLines):start]
				translated, batchFailed, err := runBatch(p, results, start, texts[start:end], before)
				record(translated, batchFailed, err)
				time.Sleep(batchSleep)
			}
		}()
	}
	for start := 0; start < total; start += batchSize {
		starts <- start
	}
	close(starts)
	wg.Wait()

	// The batches finish in whatever order the endpoint answers them, and
	// firstFew(failed, 3) is shown to the user as "first: [...]".
	slices.Sort(failed)

	if len(failed) == total {
		return results, failed, fmt.Errorf("all %d blocks failed: %w", total, lastErr)
	}
	return results, failed, nil
}

// runBatch translates one batch into results[start:], returning how many blocks
// came back translated, the absolute indices of those that did not, and the last
// error seen. It writes only inside its own range, so the workers never share a
// slice element.
func runBatch(p provider, results []string, start int, batch, before []string) (int, []int, error) {
	translated, err := p.batch(batch, before)
	if err == nil && len(translated) != len(batch) {
		// The pool's whole safety argument is that a worker writes only inside
		// its own range. A provider handing back more strings than it was given
		// would write into the next worker's range — a real data race and a
		// scrambled file, with progress still reading 100%. The provider checks its
		// own counts, so this is a backstop, and a cheap one.
		err = fmt.Errorf("provider returned %d translations for %d blocks",
			len(translated), len(batch))
	}
	if err == nil {
		copy(results[start:start+len(batch)], translated)
		return len(batch), nil, nil
	}

	// giveUp marks every block from j onwards as failed with its source kept. It
	// is how both throttling cases end: there is nothing left worth asking for.
	var failed []int
	giveUp := func(j int) []int {
		for ; j < len(batch); j++ {
			results[start+j] = batch[j]
			failed = append(failed, start+j)
		}
		return failed
	}

	var se statusError
	if errors.As(err, &se) {
		// A status-level failure (429, 5xx) has already been retried to
		// exhaustion: the endpoint is down or throttling, and single blocks
		// cannot succeed where the batch just failed four times. Fanning out
		// here is what once turned a sustained rate limit into a multi-hour wait
		// before the user saw any error at all.
		return 0, giveUp(0), err
	}

	// Any other failure — a reply whose key set is not the one the batch asked
	// about — says nothing about the endpoint's health, so each block gets its own
	// attempt. This is the expensive path, not the cheap one: the provider retries
	// its own batch first, because one more batch request is far cheaper than
	// len(batch) single ones.
	lastErr := err
	n := 0
	// statusFailures bounds what the fan-out will tolerate before deciding the
	// endpoint, rather than a block, is the problem. Carrying on regardless costs
	// maxAttempts requests for every remaining block, each attempt able to honour
	// a Retry-After, four workers deep: measured at 161 requests and over two
	// minutes for a single 40-block batch, with the user watching an unmoving
	// progress line and no error at all.
	statusFailures := 0
	for j, t := range batch {
		r, err := p.one(t, before)
		if err != nil {
			lastErr = err
			if errors.As(err, &se) {
				statusFailures++
				// A 429 is a statement about us, not about this block: every
				// further request makes the throttling worse, so the batch ends
				// here. A 5xx may genuinely be about one block's content — one bad
				// block must not cost the other 1833 — but a second status failure
				// is the endpoint, not the block.
				if se.code == http.StatusTooManyRequests || statusFailures > 1 {
					return n, giveUp(j), lastErr
				}
			}
			results[start+j] = t
			failed = append(failed, start+j)
		} else {
			results[start+j] = r
			n++
		}
		time.Sleep(perBlockSleep)
	}
	return n, failed, lastErr
}

// The two endpoint refusals a caller can do something about. They are sentinels
// rather than status codes on purpose: this package is the only place that knows
// an HTTP status at all, and a caller that had to match on "translation service
// returned HTTP 401" — as the CLI did, by hard-coding a kind at one call site
// and losing it entirely at the other — is matching on prose.
//
// Both survive %w-wrapping, so errors.Is finds them through translateAll's "all
// N blocks failed" and through the progressive run's per-chunk wrapper.
var (
	// ErrUnauthorized is the endpoint rejecting our credentials: a mistyped or
	// revoked key, or one without access to the model. Retrying cannot fix it,
	// which is exactly what made reporting it as a rate limit harmful.
	ErrUnauthorized = errors.New("the translation endpoint rejected the API key")

	// ErrRateLimited is the endpoint throttling us — HTTP 429 and nothing else.
	ErrRateLimited = errors.New("the translation endpoint is rate limiting this key")
)

// statusError carries the HTTP status so the retry logic can tell a transient
// throttle from a request that will never succeed, and the Retry-After the
// server asked for when it sent one.
type statusError struct {
	code       int
	retryAfter time.Duration
}

// Is maps the status onto the exported sentinels, which is what lets a caller
// several wraps away ask "was this an auth failure?" without knowing this type
// or the status code exists.
func (e statusError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.code == http.StatusUnauthorized || e.code == http.StatusForbidden
	case ErrRateLimited:
		return e.code == http.StatusTooManyRequests
	}
	return false
}

func (e statusError) Error() string {
	if e.code == http.StatusTooManyRequests {
		return fmt.Sprintf("rate limited by the translation service (HTTP %d)", e.code)
	}
	return fmt.Sprintf("translation service returned HTTP %d", e.code)
}

func (e statusError) retryable() bool {
	return e.code == http.StatusTooManyRequests || e.code >= 500
}

// retryAfterUnit is what one unit of a Retry-After header means. It is a package
// var for the same reason the sleeps above are: honouring a real "wait 3
// seconds" in the test suite would cost three seconds per case.
var (
	retryAfterUnit = time.Second
	// maxRetryAfter caps what a server can make a run wait. A server asking for
	// longer than half a minute is telling us to come back later, not to hold a
	// goroutine: past that the backoff and the failure report serve the user
	// better than waiting does.
	maxRetryAfter = 30 * time.Second
)

// parseRetryAfter reads the delta-seconds form of Retry-After. The HTTP-date
// form is deliberately ignored rather than guessed at: falling back to the
// widening backoff is never wrong, only slower.
func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	if d := time.Duration(secs) * retryAfterUnit; d < maxRetryAfter {
		return d
	}
	return maxRetryAfter
}

// retry runs fn until it succeeds, until it fails for a reason a repeat cannot
// fix, or until the attempts run out. A 429 is usually a short burst; a 4xx that
// is not 429 will never succeed on a repeat, so it fails immediately. When the
// server said how long to wait, that wins over the backoff.
func retry(fn func() (string, error)) (string, error) {
	var lastErr error
	delay := retryBaseDelay
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, err := fn()
		if err == nil {
			return out, nil
		}
		lastErr = err
		var se statusError
		if !errors.As(err, &se) || !se.retryable() {
			return "", err
		}
		if attempt < maxAttempts {
			wait := delay
			if se.retryAfter > 0 {
				wait = se.retryAfter
			}
			time.Sleep(wait)
			delay *= 2
		}
	}
	return "", lastErr
}
