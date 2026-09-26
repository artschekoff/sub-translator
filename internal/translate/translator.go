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
// Both providers implement it, so main.go and the progressive run are written
// against this and never against a particular endpoint.
type Translator interface {
	TranslateAll(texts []string, progress func(done, total int)) ([]string, []int, error)
}

// Provider names, as the -provider flag and translate.provider config accept them.
const (
	ProviderGoogle = "google"
	ProviderOpenAI = "openai"
)

const (
	// DefaultModel is the OpenAI model subtitle translation defaults to. Of the
	// current line — Astra (flagship, complex reasoning), Sol (balanced), Luna
	// (cost-sensitive, high volume) — subtitles are the high-volume,
	// low-reasoning case, which is Luna.
	DefaultModel = "gpt-6-luna"

	// DefaultWorkers is how many batches are translated at once. Four is enough
	// to hide the round-trip latency that used to dominate a run without turning
	// a free endpoint's rate limit into a wall.
	DefaultWorkers = 4

	// contextLines is how many already-seen source lines are handed to an LLM
	// provider as context. Three is enough to carry who is addressing whom,
	// names and formality across a batch boundary.
	contextLines = 3
)

// Options describes the translator a run wants. It is deliberately a plain
// struct: main.go resolves flags over config over defaults into one of these,
// and the end-to-end tests fill in BaseURL to keep a whole run off the network.
type Options struct {
	Provider string // "google" or "openai"
	From, To string
	APIKey   string
	Model    string
	Workers  int
	BaseURL  string // test seam; empty means the provider's real endpoint
}

// Validate reports the option errors worth raising before any work happens. It
// is exported separately from NewTranslator so a run can refuse a missing API
// key up front, rather than after an hour of transcription.
func (o Options) Validate() error {
	switch o.Provider {
	case "", ProviderGoogle:
		return nil
	case ProviderOpenAI:
		if strings.TrimSpace(o.APIKey) == "" {
			return errors.New("the openai provider needs an API key: " +
				"set OPENAI_API_KEY in the environment, or run " +
				"`sub-translator config set translate.api-key <key>`")
		}
		return nil
	default:
		return fmt.Errorf("unknown translation provider %q; valid providers: %s, %s",
			o.Provider, ProviderGoogle, ProviderOpenAI)
	}
}

// NewTranslator builds the provider the options name. An empty provider, model
// or worker count takes the default, so a partially configured machine still
// runs.
func NewTranslator(o Options) (Translator, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if o.Workers < 1 {
		o.Workers = DefaultWorkers
	}
	switch o.Provider {
	case "", ProviderGoogle:
		c := New(o.From, o.To)
		if o.BaseURL != "" {
			c.baseURL = o.BaseURL
		}
		c.Workers = o.Workers
		return c, nil
	default: // Validate has already rejected anything but openai here.
		return newOpenAI(o), nil
	}
}

// provider is one backend as the worker pool sees it. Keeping the pool blind to
// which endpoint is behind it is what lets the ordering, progress and
// failure-accounting rules be written once.
type provider interface {
	// batch translates every text in one request, retries included. before holds
	// the source lines immediately preceding the batch, as context for a provider
	// that can use it.
	batch(texts, before []string) ([]string, error)
	// one translates a single text, for when a batch comes back unusable.
	one(text string, before []string) (string, error)
}

// translateAll is the shared body of every provider's TranslateAll: it splits
// the texts into batches, translates up to workers of them at a time, and
// accounts for what failed.
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
		// scrambled file, with progress still reading 100%. Both providers check
		// their own counts, so this is a backstop, and a cheap one.
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

	// Any other failure — a mangled separator on the Google side, a key set that
	// does not match on the OpenAI side — says nothing about the endpoint's
	// health, so each block gets its own attempt.
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

// statusError carries the HTTP status so the retry logic can tell a transient
// throttle from a request that will never succeed, and the Retry-After the
// server asked for when it sent one.
type statusError struct {
	code       int
	retryAfter time.Duration
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
