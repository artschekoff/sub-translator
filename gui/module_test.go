package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
//
// It walks gui/ recursively (eight more tasks add files here) and parses each
// file's real import declarations with go/parser rather than scanning source
// text for a substring. A text scan would also fire on a comment or doc
// string that merely mentions the CLI's import path. go list -deps was
// considered and rejected: it only reports the file set selected for the
// current GOOS/GOARCH, so an import hidden behind a build tag for a platform
// the test runner isn't on would pass unseen — and this repository already
// ships build-tagged files elsewhere (internal/proc). go/parser reads every
// file's ImportSpec list regardless of build constraints, matches only real
// imports, and needs the module to parse, not compile.
func TestTheGUIDoesNotImportTheCLI(t *testing.T) {
	const forbidden = "github.com/artschekoff/sub-translator/internal"

	fset := token.NewFileSet()
	scanned := 0

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Nothing under gui/ should be skipped by name today, but keep
			// this walk cheap and out of VCS internals if it's ever run
			// against a tree that has them.
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}

		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		scanned++

		for _, imp := range f.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if importPath == forbidden || strings.HasPrefix(importPath, forbidden+"/") {
				t.Errorf("%s imports the CLI's internal packages: %s", path, importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A scan that found no files would pass silently.
	if scanned == 0 {
		t.Fatal("scanned no .go files in gui/")
	}
}

// trackedFiles returns every path git considers tracked in dir's working
// tree, relative to dir.
func trackedFiles(dir string) ([]string, error) {
	out, err := exec.Command("git", "-C", dir, "ls-files", "--", ".").Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, path := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if path != "" {
			files = append(files, path)
		}
	}
	return files, nil
}

// trackedNodeFiles reports which of dir's tracked files (per trackedFiles)
// are a node toolchain artifact (see isNodeToolchainPath). It takes a
// directory rather than assuming the current one so it can be pointed at a
// disposable fixture repository in a test, not only at this module's own
// working tree.
func trackedNodeFiles(dir string) ([]string, error) {
	files, err := trackedFiles(dir)
	if err != nil {
		return nil, err
	}

	var violations []string
	for _, path := range files {
		if isNodeToolchainPath(path) {
			violations = append(violations, path)
		}
	}
	return violations, nil
}

// isNodeToolchainPath reports whether a slash-separated path is a node
// toolchain artifact: package.json, one of its common lockfiles, or anything
// under a node_modules/ directory.
//
// The predicate is shared by the two guards that need it, deliberately. The
// "no node toolchain" constraint has two surfaces — what is committed
// (TestTheTrackedTreeHasNoNodeToolchain) and what is shipped
// (TestTheEmbeddedAssetsHaveNoNodeToolchain) — and a second copy of this list
// would let one surface start accepting a lockfile the other still rejects.
func isNodeToolchainPath(path string) bool {
	nodeFiles := map[string]bool{
		"package.json":      true,
		"package-lock.json": true,
		"yarn.lock":         true,
		"pnpm-lock.yaml":    true,
		"bun.lockb":         true,
	}

	if nodeFiles[filepath.Base(path)] {
		return true
	}
	return path == "node_modules" || strings.HasPrefix(path, "node_modules/") ||
		strings.Contains(path, "/node_modules/")
}

// TestTheEmbeddedAssetsHaveNoNodeToolchain guards the shipped artifact, which
// is a different surface from the tracked tree and was unguarded until now.
//
// TestTheTrackedTreeHasNoNodeToolchain reads `git ls-files`, by design: a
// generated file sitting untracked next to the source is expected and fine.
// But //go:embed does not consult git. A directory-form `//go:embed frontend`
// takes everything under frontend/ whose path elements do not begin with "."
// or "_", and `wails build` generates frontend/wailsjs/ there — gitignored,
// with its own package.json. So the tracked-tree guard passed truthfully
// while the built binary carried a package.json inside it, in a project whose
// constraints forbid a node toolchain. Nothing looked at the artifact.
//
// This walks embeddedAssets itself, so it sees exactly what ships. It is
// insensitive to how the directive is spelled: it fails for the directory
// form on any machine where `wails build` has run, and it keeps failing if a
// later asset drags a lockfile in with it.
func TestTheEmbeddedAssetsHaveNoNodeToolchain(t *testing.T) {
	var embedded []string

	err := fs.WalkDir(embeddedAssets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		embedded = append(embedded, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A walk that found nothing would pass silently, and an empty embed is its
	// own defect: the window would start and serve a blank page.
	if len(embedded) == 0 {
		t.Fatal("the embedded asset set is empty")
	}

	for _, path := range embedded {
		if isNodeToolchainPath(path) {
			t.Errorf("the shipped binary embeds a node toolchain artifact: %s\n"+
				"embedded set: %v\n"+
				"a directory-form //go:embed takes frontend/wailsjs/ with it; "+
				"name the asset files instead", path, embedded)
		}
	}

	// The three files the window actually loads must be in there, or the embed
	// directive has been narrowed past the point of working.
	for _, want := range []string{
		"frontend/index.html",
		"frontend/app.css",
		"frontend/app.js",
	} {
		if !slices.Contains(embedded, want) {
			t.Errorf("%s is not embedded; the window would serve a 404 for it\nembedded set: %v",
				want, embedded)
		}
	}
}

// TestTheTrackedTreeHasNoNodeToolchain guards the plan-wide "no node
// toolchain" constraint: gui/frontend/index.html is a plain file, no
// bundler, no framework, nothing generated by `npm install`. `wails build`
// itself generates gui/frontend/wailsjs/ (with its own package.json) and
// gui/build/; gui/.gitignore keeps those untracked, but the ignore file only
// works as long as nobody overrides it with `git add -A` or `git add -f`.
// This test makes that failure mode loud: it inspects the tracked set via
// `git ls-files`, not the filesystem, because a generated file sitting
// untracked next to the source is expected and fine — only a committed one
// breaks the constraint.
//
// It reads .git/index before shelling out to git, purely so this test's own
// result depends on the tracked set. Go's test cache keys a cached result on
// the files the test opens, not on a subprocess's output, so without this
// read, committing a violating file and rerunning `go test` (without
// -count=1) can report a stale cached PASS from before the commit — a
// constraint whose guard says "fine" while being violated is worse than no
// guard. Reading .git/index puts that file's content in the cache key, so Go
// invalidates the cached result whenever the index changes: on every add,
// commit, or reset.
func TestTheTrackedTreeHasNoNodeToolchain(t *testing.T) {
	if _, err := os.ReadFile(filepath.Join("..", ".git", "index")); err != nil {
		t.Fatal(err)
	}

	all, err := trackedFiles(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("git ls-files returned nothing for gui/ — is this running inside the git repo?")
	}

	violations, err := trackedNodeFiles(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		t.Errorf("tracked node toolchain artifact: %s", v)
	}
}

// TestTrackedNodeFilesFixture exercises trackedNodeFiles against a
// disposable git repository instead of this module's own tree, so the guard
// stays verifiable forever without ever committing a violation to this
// repository's real history (which is how round 1's and round 2's mutation
// verification each left a paired scratch/revert commit behind).
func TestTrackedNodeFilesFixture(t *testing.T) {
	dir := t.TempDir()

	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "fixture@example.invalid")
	runGit(t, dir, "config", "user.name", "fixture")
	runGit(t, dir, "config", "commit.gpgsign", "false")

	writeFile(t, filepath.Join(dir, "index.html"), "<html></html>")
	runGit(t, dir, "add", "index.html")
	runGit(t, dir, "commit", "-q", "-m", "clean tree")

	violations, err := trackedNodeFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("clean fixture tree flagged as violating: %v", violations)
	}

	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"fixture"}`)
	runGit(t, dir, "add", "package.json")
	runGit(t, dir, "commit", "-q", "-m", "add package.json")

	violations, err = trackedNodeFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 || violations[0] != "package.json" {
		t.Fatalf("expected exactly [package.json] to be flagged, got %v", violations)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
