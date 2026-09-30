package main

import (
	"os"
	"strings"
	"testing"
)

// TestTheRootModuleStaysDependencyFree is the constraint this whole module
// exists to protect: Wails needs cgo and a dozen dependencies, and the CLI
// cross-compiles to five platforms because it has none. Keeping them in
// separate modules is what lets both be true.
func TestTheRootModuleStaysDependencyFree(t *testing.T) {
	body, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "require") {
		t.Errorf("the root go.mod has a require block:\n%s\n"+
			"Wails belongs in gui/go.mod. The root module cross-compiles to five "+
			"platforms and cannot take a cgo dependency.", body)
	}
}

// TestTheGUIDoesNotImportTheCLI pins the other half of the separation: the
// window drives the binary through its event stream, so importing its packages
// would couple them and drag the GUI's build constraints back into the CLI.
func TestTheGUIDoesNotImportTheCLI(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// Built by concatenation, not as one literal: written out whole, this
	// string would appear verbatim in this file's own source (right here in
	// the test), and the scan below would flag module_test.go for "importing"
	// what it's merely checking for.
	forbidden := strings.Join([]string{"github.com/artschekoff/sub-translator", "internal"}, "/")
	scanned := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		body, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		if strings.Contains(string(body), forbidden) {
			t.Errorf("%s imports the CLI's internal packages", e.Name())
		}
	}
	// A scan that found no files would pass silently.
	if scanned == 0 {
		t.Fatal("scanned no .go files in gui/")
	}
}
