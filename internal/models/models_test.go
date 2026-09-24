package models

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The catalog must name models exactly as whisper.cpp does, because the
// filename it produces has to match what other whisper.cpp front-ends already
// have on disk — that is the whole of "compatibility" here.
func TestCatalogFilenamesFollowGGMLConvention(t *testing.T) {
	for _, m := range Catalog() {
		if !strings.HasPrefix(m.Filename, "ggml-") || !strings.HasSuffix(m.Filename, ".bin") {
			t.Errorf("%s: filename %q is not ggml-<name>.bin", m.Name, m.Filename)
		}
		if !strings.HasSuffix(m.URL, m.Filename) {
			t.Errorf("%s: URL %q does not end in its filename %q", m.Name, m.URL, m.Filename)
		}
	}
}

// large-v3-turbo and the Silero VAD model are the two files already present in
// the author's whisper.cpp setup; both must be in the catalog and must resolve
// to exactly those filenames.
func TestCatalogCoversTheModelsAlreadyOnDisk(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		kind     Kind
	}{
		{"large-v3-turbo", "ggml-large-v3-turbo.bin", KindTranscribe},
		{"silero-vad", "ggml-silero-v5.1.2.bin", KindVAD},
		{"base", "ggml-base.bin", KindTranscribe},
	}
	for _, tt := range tests {
		m, ok := Find(tt.name)
		if !ok {
			t.Errorf("Find(%q): not in catalog", tt.name)
			continue
		}
		if m.Filename != tt.filename {
			t.Errorf("Find(%q).Filename = %q, want %q", tt.name, m.Filename, tt.filename)
		}
		if m.Kind != tt.kind {
			t.Errorf("Find(%q).Kind = %v, want %v", tt.name, m.Kind, tt.kind)
		}
	}
	if _, ok := Find("gpt-4"); ok {
		t.Error("Find: want miss for a name that is not a whisper model")
	}
}

// A VAD model is not a transcription model. Feeding Silero to -m makes
// whisper-cli fail with an opaque tensor error, so discovery must keep the two
// kinds apart.
func TestIsVADFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"ggml-silero-v5.1.2.bin", true},
		{"/abs/path/ggml-silero-v5.1.2.bin", true},
		{"ggml-vad.bin", true},
		{"ggml-large-v3-turbo.bin", false},
		{"ggml-base.en.bin", false},
	}
	for _, tt := range tests {
		if got := IsVADFile(tt.name); got != tt.want {
			t.Errorf("IsVADFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDiscoverSplitsModelsByKindAndSkipsOtherFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"ggml-large-v3-turbo.bin",
		"ggml-silero-v5.1.2.bin",
		"notes.txt",
		"ggml-base.bin.part", // an interrupted download must never be offered
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	transcribe, vad := Discover([]string{dir, filepath.Join(dir, "does-not-exist")})

	if len(transcribe) != 1 || filepath.Base(transcribe[0]) != "ggml-large-v3-turbo.bin" {
		t.Errorf("transcribe models = %v, want just ggml-large-v3-turbo.bin", transcribe)
	}
	if len(vad) != 1 || filepath.Base(vad[0]) != "ggml-silero-v5.1.2.bin" {
		t.Errorf("vad models = %v, want just ggml-silero-v5.1.2.bin", vad)
	}
}

// The configured directory must win, and our own directory must be consulted
// before another application's, so `model pull` output is preferred over a
// file we do not control.
func TestSearchDirsOrder(t *testing.T) {
	dirs := SearchDirs("/configured")
	if len(dirs) == 0 || dirs[0] != "/configured" {
		t.Fatalf("configured dir must come first, got %v", dirs)
	}
	if slices.Contains(SearchDirs(""), "/configured") {
		t.Error("empty configured dir must not appear in the search path")
	}

	if runtime.GOOS != "darwin" {
		return
	}
	own, err := DefaultDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	iOwn := slices.Index(dirs, own)
	iSlaive := -1
	for i, d := range dirs {
		if strings.Contains(d, "net.slaive.app") {
			iSlaive = i
		}
	}
	if iOwn < 0 {
		t.Fatalf("our own model dir %q missing from %v", own, dirs)
	}
	if iSlaive < 0 {
		t.Fatalf("the Slaive model dir is missing from %v", dirs)
	}
	if iOwn > iSlaive {
		t.Errorf("our dir (%d) must precede Slaive's (%d): %v", iOwn, iSlaive, dirs)
	}
}

// A pull that dies halfway must not leave a truncated .bin behind, because
// Discover would happily hand it to whisper on the next run.
func TestPullRejectsShortBodyAndLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	srv := shortBodyServer(t)
	m := Model{Name: "test", Filename: "ggml-test.bin", URL: srv, Kind: KindTranscribe}

	if _, err := Pull(m, dir, nil); err == nil {
		t.Fatal("want error when the body is shorter than Content-Length")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed pull left files behind: %v", entries)
	}
}

func TestPullWritesFileAndReportsProgress(t *testing.T) {
	dir := t.TempDir()
	srv := okServer(t, []byte("hello whisper"))
	m := Model{Name: "test", Filename: "ggml-test.bin", URL: srv, Kind: KindTranscribe}

	var lastDone, lastTotal int64
	path, err := Pull(m, dir, func(done, total int64) { lastDone, lastTotal = done, total })
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if got := filepath.Base(path); got != "ggml-test.bin" {
		t.Errorf("saved as %q, want ggml-test.bin", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello whisper" {
		t.Errorf("content = %q", data)
	}
	if lastTotal != int64(len("hello whisper")) || lastDone != lastTotal {
		t.Errorf("progress ended at %d/%d, want %d/%d",
			lastDone, lastTotal, lastTotal, lastTotal)
	}
}

func okServer(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// shortBodyServer promises more bytes than it delivers, which is what a dropped
// connection looks like to the client.
func shortBodyServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("truncated"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
