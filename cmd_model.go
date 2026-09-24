package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/artschekoff/sub-translator/internal/config"
	"github.com/artschekoff/sub-translator/internal/models"
)

const modelUsage = `Usage:
  sub-translator model list          show available models and which are installed
  sub-translator model pull <name>   download a model`

func runModel(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("model: missing subcommand\n\n%s", modelUsage)
	}

	switch args[0] {
	case "list":
		return listModels(out)

	case "pull":
		if len(args) != 2 {
			return fmt.Errorf("model pull: want exactly one model name\n\n%s", modelUsage)
		}
		return pullModel(args[1], out)

	default:
		return fmt.Errorf("model: unknown subcommand %q\n\n%s", args[0], modelUsage)
	}
}

func listModels(out io.Writer) error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	dirs := models.SearchDirs(c.Models.Dir)
	transcribe, vad := models.Discover(dirs)

	installed := map[string]string{}
	for _, p := range append(append([]string{}, transcribe...), vad...) {
		installed[filepath.Base(p)] = p
	}

	fmt.Fprintln(out, "Available models:")
	for _, m := range models.Catalog() {
		status := ""
		if path, ok := installed[m.Filename]; ok {
			status = "installed: " + path
		}
		fmt.Fprintf(out, "  %-16s %-8s %s\n", m.Name, m.Kind, status)
	}

	fmt.Fprintln(out, "\nSearched directories:")
	for _, d := range dirs {
		fmt.Fprintf(out, "  %s\n", d)
	}
	return nil
}

func pullModel(name string, out io.Writer) error {
	m, ok := models.Find(name)
	if !ok {
		return fmt.Errorf("unknown model %q — run `sub-translator model list` to see the options", name)
	}

	c, err := config.Load()
	if err != nil {
		return err
	}
	dest := c.Models.Dir
	if dest == "" {
		if dest, err = models.DefaultDir(); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "Downloading %s → %s\n", m.Name, filepath.Join(dest, m.Filename))
	lastPct := -1
	path, err := models.Pull(m, dest, func(done, total int64) {
		if total <= 0 {
			return
		}
		if pct := int(done * 100 / total); pct != lastPct {
			lastPct = pct
			fmt.Fprintf(os.Stderr, "\r  progress: %d%% (%d/%d MB)   ",
				pct, done/(1<<20), total/(1<<20))
		}
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Saved: %s\n", path)
	return nil
}

// dispatchSubcommand runs a subcommand when args start with one. It reports
// whether it handled the call, so main can fall through to normal flag parsing
// for an ordinary translation run.
func dispatchSubcommand(args []string, out io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "config":
		return true, runConfig(args[1:], out)
	case "model":
		return true, runModel(args[1:], out)
	default:
		return false, nil
	}
}
