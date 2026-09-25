package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultAPIURL = "https://translate.googleapis.com/translate_a/single"
	separator     = " ||||| "
	batchSize     = 40
	maxAttempts   = 4
)

// Package-level vars allow tests to override timing without changing production behavior.
var (
	retryBaseDelay = time.Second
	perBlockSleep  = 150 * time.Millisecond
	batchSleep     = 300 * time.Millisecond
)

type Client struct {
	From    string
	To      string
	baseURL string
	http    *http.Client
}

func New(from, to string) *Client {
	return &Client{
		From:    from,
		To:      to,
		baseURL: defaultAPIURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// TranslateAll translates every text, returning the results, the indices of any
// blocks that could not be translated and kept their original text, and an error
// only when nothing at all could be translated.
//
// Reporting a total failure as success is how an untranslated file used to end up
// named .es.srt: every block silently kept its English text while the progress
// counter reached 100%.
func (c *Client) TranslateAll(texts []string, progress func(done, total int)) ([]string, []int, error) {
	results := make([]string, len(texts))
	var failed []int
	var lastErr error

	for i := 0; i < len(texts); i += batchSize {
		end := min(i+batchSize, len(texts))
		batch := texts[i:end]

		translated, err := c.translateBatch(batch)
		if err == nil {
			copy(results[i:], translated)
		} else {
			lastErr = err
			// Distinguish: if the batch failed with a status error (429, 5xx),
			// the endpoint is down or throttling us. Per-block retries cannot
			// succeed where the batch just failed four times. Mark the entire
			// batch as failed and move on. Only attempt per-block retries if
			// the batch failed for a different reason (e.g., separator mismatch).
			var se statusError
			if errors.As(err, &se) {
				// Status-level failure already retried; don't retry per-block
				for j := range batch {
					results[i+j] = batch[j]
					failed = append(failed, i+j)
				}
			} else {
				// Other failure (e.g., separator mismatch); try per-block retries
				for j, t := range batch {
					r, err := c.translateWithRetry(t)
					if err != nil {
						results[i+j] = t
						failed = append(failed, i+j)
						lastErr = err
					} else {
						results[i+j] = r
					}
					time.Sleep(perBlockSleep)
				}
			}
		}
		if progress != nil {
			progress(end, len(texts))
		}
		time.Sleep(batchSleep)
	}

	if len(failed) == len(texts) && len(texts) > 0 {
		return results, failed, fmt.Errorf("all %d blocks failed: %w", len(texts), lastErr)
	}
	return results, failed, nil
}

// statusError carries the HTTP status so the retry logic can tell a transient
// throttle from a request that will never succeed.
type statusError struct{ code int }

func (e statusError) Error() string {
	if e.code == http.StatusTooManyRequests {
		return fmt.Sprintf("rate limited by the translation service (HTTP %d)", e.code)
	}
	return fmt.Sprintf("translation service returned HTTP %d", e.code)
}

func (e statusError) retryable() bool {
	return e.code == http.StatusTooManyRequests || e.code >= 500
}

// translateWithRetry retries throttling and server faults with a widening pause.
// A 429 is usually a short burst; a 4xx that is not 429 will never succeed on a
// repeat, so it fails immediately.
func (c *Client) translateWithRetry(text string) (string, error) {
	var lastErr error
	delay := retryBaseDelay
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, err := c.translateOne(text)
		if err == nil {
			return out, nil
		}
		lastErr = err
		var se statusError
		if !errors.As(err, &se) || !se.retryable() {
			return "", err
		}
		if attempt < maxAttempts {
			time.Sleep(delay)
			delay *= 2
		}
	}
	return "", lastErr
}

func (c *Client) translateBatch(texts []string) ([]string, error) {
	joined := strings.Join(texts, separator)
	result, err := c.translateWithRetry(joined)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(result, strings.TrimSpace(separator))
	if len(parts) != len(texts) {
		// separator may have been altered by translation — fall back
		return nil, fmt.Errorf("separator mismatch: got %d parts, want %d", len(parts), len(texts))
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts, nil
}

func (c *Client) translateOne(text string) (string, error) {
	params := url.Values{
		"client": {"gtx"},
		"sl":     {c.From},
		"tl":     {c.To},
		"dt":     {"t"},
		"q":      {text},
	}
	resp, err := c.http.Get(c.baseURL + "?" + params.Encode())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", statusError{code: resp.StatusCode}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	// Response: [[[translated, original, ...], ...], null, srcLang]
	var raw [][][]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		// Try broader parse
		var fallback []interface{}
		if err2 := json.Unmarshal(body, &fallback); err2 != nil {
			return "", fmt.Errorf("parse response: %w", err)
		}
		return extractText(fallback), nil
	}
	var sb strings.Builder
	for _, pair := range raw[0] {
		if len(pair) > 0 {
			if s, ok := pair[0].(string); ok {
				sb.WriteString(s)
			}
		}
	}
	return sb.String(), nil
}

func extractText(v []interface{}) string {
	// Walk nested arrays and collect first string from innermost [translated, original] pairs
	var sb strings.Builder
	if len(v) == 0 {
		return ""
	}
	first, ok := v[0].([]interface{})
	if !ok {
		return ""
	}
	for _, item := range first {
		pair, ok := item.([]interface{})
		if !ok || len(pair) == 0 {
			continue
		}
		if s, ok := pair[0].(string); ok {
			sb.WriteString(s)
		}
	}
	return sb.String()
}
