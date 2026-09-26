package translate

import (
	"encoding/json"
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
	Workers int
	baseURL string
	http    *http.Client
}

func New(from, to string) *Client {
	return &Client{
		From: from,
		To:   to,
		// One worker: New is the plain constructor, and a caller that has not
		// asked for parallelism gets exactly the sequential run this package
		// always did. NewTranslator is where the -workers default is applied.
		Workers: 1,
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
	return translateAll(c, texts, c.Workers, progress)
}

// batch and one make the Google client a provider the shared worker pool can
// drive. Google has no notion of context lines, so before is ignored.
func (c *Client) batch(texts, _ []string) ([]string, error) {
	return c.translateBatch(texts)
}

func (c *Client) one(text string, _ []string) (string, error) {
	return c.translateWithRetry(text)
}

// translateWithRetry retries throttling and server faults with a widening pause.
func (c *Client) translateWithRetry(text string) (string, error) {
	return retry(func() (string, error) { return c.translateOne(text) })
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
		return "", statusError{code: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header)}
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
