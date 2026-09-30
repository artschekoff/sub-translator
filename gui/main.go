// Command gui is the desktop window for sub-translator. It drives the CLI
// binary as a subprocess through its JSON Lines event stream on stderr; it
// never imports the CLI's packages (see module_test.go).
//
// This is the smallest app that opens a window. Task 7 replaces the options
// block below with the real layout (translucency, drag-and-drop, etc.).
package main

import (
	"embed"
	"io/fs"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

// Note: without an "all:" prefix, go:embed silently drops any file in
// frontend/ whose name starts with "." or "_", and drops an entire
// subdirectory whose only contents are such a file — the asset would build
// cleanly but simply be missing at runtime.
//
//go:embed frontend
var embeddedAssets embed.FS

func main() {
	assets, err := fs.Sub(embeddedAssets, "frontend")
	if err != nil {
		log.Fatal(err)
	}

	err = wails.Run(&options.App{
		Title:  "sub-translator",
		Width:  800,
		Height: 600,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
