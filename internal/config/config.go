// Package config stores the settings sub-translator remembers between runs —
// chiefly where the whisper binary and its ggml model live, so those paths are
// typed once rather than on every invocation.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type WhisperConfig struct {
	Bin          string  `json:"bin,omitempty"`
	Model        string  `json:"model,omitempty"`
	VADModel     string  `json:"vadModel,omitempty"`
	VADThreshold float64 `json:"vadThreshold,omitempty"`
	Threads      int     `json:"threads,omitempty"`
	Language     string  `json:"language,omitempty"`
}

type ModelsConfig struct {
	Dir string `json:"dir,omitempty"`
}

type Config struct {
	Whisper WhisperConfig `json:"whisper"`
	Models  ModelsConfig  `json:"models"`
}

// field couples a dotted key to its accessors, so Keys, Get and Set can never
// drift out of sync the way three parallel switch statements would.
type field struct {
	get func(*Config) string
	set func(*Config, string) error
}

var fields = map[string]field{
	"whisper.bin": {
		get: func(c *Config) string { return c.Whisper.Bin },
		set: func(c *Config, v string) error { c.Whisper.Bin = expandPath(v); return nil },
	},
	"whisper.model": {
		get: func(c *Config) string { return c.Whisper.Model },
		set: func(c *Config, v string) error { c.Whisper.Model = expandPath(v); return nil },
	},
	"whisper.vad-model": {
		get: func(c *Config) string { return c.Whisper.VADModel },
		set: func(c *Config, v string) error { c.Whisper.VADModel = expandPath(v); return nil },
	},
	"whisper.vad-threshold": {
		get: func(c *Config) string {
			if c.Whisper.VADThreshold == 0 {
				return ""
			}
			return strconv.FormatFloat(c.Whisper.VADThreshold, 'g', -1, 64)
		},
		set: func(c *Config, v string) error {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 || f > 1 {
				return fmt.Errorf("whisper.vad-threshold must be a number between 0 and 1, got %q", v)
			}
			c.Whisper.VADThreshold = f
			return nil
		},
	},
	"whisper.threads": {
		get: func(c *Config) string {
			if c.Whisper.Threads == 0 {
				return ""
			}
			return strconv.Itoa(c.Whisper.Threads)
		},
		set: func(c *Config, v string) error {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return fmt.Errorf("whisper.threads must be a non-negative integer, got %q", v)
			}
			c.Whisper.Threads = n
			return nil
		},
	},
	"whisper.language": {
		get: func(c *Config) string { return c.Whisper.Language },
		set: func(c *Config, v string) error { c.Whisper.Language = strings.TrimSpace(v); return nil },
	},
	"models.dir": {
		get: func(c *Config) string { return c.Models.Dir },
		set: func(c *Config, v string) error { c.Models.Dir = expandPath(v); return nil },
	},
}

// Keys returns every valid config key, sorted, for `config list` and for the
// "did you mean" list in errors.
func Keys() []string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func unknownKey(key string) error {
	return fmt.Errorf("unknown config key %q; valid keys: %s", key, strings.Join(Keys(), ", "))
}

func (c *Config) Get(key string) (string, error) {
	f, ok := fields[key]
	if !ok {
		return "", unknownKey(key)
	}
	return f.get(c), nil
}

func (c *Config) Set(key, value string) error {
	f, ok := fields[key]
	if !ok {
		return unknownKey(key)
	}
	return f.set(c, value)
}

// expandPath resolves a leading ~ and makes the path absolute, so what lands in
// the file means the same thing from any working directory.
func expandPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// Path is the config file location: $XDG_CONFIG_HOME/sub-translator/config.json,
// falling back to ~/.config. The same layout is used on every OS deliberately —
// os.UserConfigDir would put it under ~/Library on macOS, which is harder to
// find and to talk about in documentation.
func Path() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "sub-translator", "config.json"), nil
}

// Load reads the config file. A missing file yields an empty config and no
// error: that is simply a machine where nothing has been configured yet.
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &c, nil
}

func (c *Config) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
