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
	apiURL    = "https://translate.googleapis.com/translate_a/single"
	separator = " ||||| "
	batchSize = 40
)

type Client struct {
	From string
	To   string
	http *http.Client
}

func New(from, to string) *Client {
	return &Client{
		From: from,
		To:   to,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) TranslateAll(texts []string, progress func(done, total int)) ([]string, error) {
	results := make([]string, len(texts))
	for i := 0; i < len(texts); i += batchSize {
		end := i + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[i:end]
		translated, err := c.translateBatch(batch)
		if err != nil {
			// fallback: one by one
			for j, t := range batch {
				r, err2 := c.translateOne(t)
				if err2 != nil {
					results[i+j] = t // keep original on error
				} else {
					results[i+j] = r
				}
				time.Sleep(150 * time.Millisecond)
			}
		} else {
			copy(results[i:], translated)
		}
		if progress != nil {
			progress(end, len(texts))
		}
		time.Sleep(300 * time.Millisecond)
	}
	return results, nil
}

func (c *Client) translateBatch(texts []string) ([]string, error) {
	joined := strings.Join(texts, separator)
	result, err := c.translateOne(joined)
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
	resp, err := c.http.Get(apiURL + "?" + params.Encode())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
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
