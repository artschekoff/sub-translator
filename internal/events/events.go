// Package events carries the machine-readable stream the CLI writes under
// -events json: one JSON object per line, so a wrapper can drive the tool
// without parsing output meant for a person.
//
// The stream is deliberately one-way. The caller states its intent in argv and
// reads what happened; nothing here asks the caller a question, which is what
// separates this from a JSON-RPC protocol.
package events

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// ProtocolVersion is carried by the first event of every run. A consumer that
// understands fewer versions than it sees should say so rather than guess.
const ProtocolVersion = 1

type Kind string

const (
	KindVersion  Kind = "version"
	KindProbe    Kind = "probe"
	KindStage    Kind = "stage"
	KindProgress Kind = "progress"
	KindLanguage Kind = "language"
	KindSaved    Kind = "saved"
	KindWarning  Kind = "warning"
	KindDone     Kind = "done"
	KindError    Kind = "error"
)

// Stream is one audio or subtitle track as the probe reports it.
type Stream struct {
	Index int    `json:"index"`
	Lang  string `json:"lang"`
	Title string `json:"title"`
	Codec string `json:"codec"`
}

// base is embedded by every event so the type tag and timestamp are written the
// same way everywhere and cannot be forgotten at a call site.
type base struct {
	Type Kind   `json:"type"`
	T    string `json:"t"`
}

func (b *base) setBase(k Kind, t time.Time) {
	b.Type = k
	b.T = t.UTC().Format(time.RFC3339)
}

// Event is implemented by embedding base. The unexported methods keep the set
// closed to this package, so a consumer's switch cannot be surprised.
type Event interface {
	kind() Kind
	setBase(Kind, time.Time)
}

type Version struct {
	base
	Events int    `json:"events"`
	Tool   string `json:"tool"`
}

func (*Version) kind() Kind { return KindVersion }

type Probe struct {
	base
	Duration  float64   `json:"duration,omitempty"`
	Audio     []Stream  `json:"audio"`
	Subtitles []Stream  `json:"subtitles"`
	Fast      *FastRule `json:"fast,omitempty"`
}

func (*Probe) kind() Kind { return KindProbe }

// FastRule is the condition under which -fast is accepted, stated so a caller
// can disable a control with a reason rather than copying the rule.
type FastRule struct {
	Sources         []string `json:"sources"`
	Modes           []string `json:"modes"`
	MinChunkMinutes int      `json:"min_chunk_minutes"`
}

type Stage struct {
	base
	Name  string `json:"name"`
	Chunk int    `json:"chunk,omitempty"`
	Of    int    `json:"of,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
}

func (*Stage) kind() Kind { return KindStage }

type Progress struct {
	base
	Pct int `json:"pct"`
}

func (*Progress) kind() Kind { return KindProgress }

type Language struct {
	base
	Detected string `json:"detected"`
}

func (*Language) kind() Kind { return KindLanguage }

type Saved struct {
	base
	Path      string `json:"path"`
	Covers    string `json:"covers"`
	Watchable bool   `json:"watchable"`
}

func (*Saved) kind() Kind { return KindSaved }

type Warning struct {
	base
	Kind  string `json:"kind"`
	Count int    `json:"count"`
	Total int    `json:"total"`
	First []int  `json:"first,omitempty"`
}

func (*Warning) kind() Kind { return KindWarning }

type Done struct {
	base
	Path string `json:"path"`
	// Covers is the clock value the -fast path knows and the ordinary run does
	// not: the ordinary run translates whatever it parsed in one pass, not a
	// span of the container's own runtime, so there is no honest value to put
	// here. omitempty makes that absence explicit — "unknown", never "covers
	// nothing" — instead of shipping a coverage claim of "".
	Covers string `json:"covers,omitempty"`
}

func (*Done) kind() Kind { return KindDone }

type Error struct {
	base
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (*Error) kind() Kind { return KindError }

// SyncWriter serialises writes to one underlying writer and remembers whether
// the stream is at the start of a line. The event stream and the prose the tool
// writes for a person share a file descriptor by design, and they are written
// from different goroutines — the SIGINT handler emits while the main goroutine
// may be halfway through a warning line, or waiting for an answer to a prompt.
// Routing both through one of these makes every write of ours whole: a consumer
// sees a complete JSON object or a complete prose line, never one inside the
// other.
//
// Two mechanisms are needed for that, and the second one was learned the hard
// way. The lock makes each write atomic, which is what keeps two writers out of
// each other's bytes. It says nothing about column position, so an event written
// after a prompt — "Target language code: ", deliberately without a newline,
// because the answer is typed on the same line — still began on that line and
// still could not be parsed. So this also tracks the last byte written, and
// writeOnItsOwnLine breaks the line first when the stream is mid-line.
//
// It guards only our own writes. A subprocess writing the same descriptor cannot
// be locked out, which is why the ffmpeg children have their stderr captured
// instead (see internal/media).
type SyncWriter struct {
	mu sync.Mutex
	w  io.Writer
	// atLineStart is true when nothing has been written yet or the last byte
	// written was a newline. A carriage return does not count: after a
	// "size=… \r" fragment a reader splitting on newlines still sees whatever
	// comes next as part of that line, which is the whole failure being
	// prevented.
	atLineStart bool
}

func NewSyncWriter(w io.Writer) *SyncWriter {
	return &SyncWriter{w: w, atLineStart: true}
}

func (s *SyncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(p)
}

// write is Write's body with the lock already held, so writeOnItsOwnLine can
// make two writes without anything landing between them.
func (s *SyncWriter) write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if n > 0 {
		s.atLineStart = p[n-1] == '\n'
	}
	return n, err
}

// writeOnItsOwnLine writes p, preceded by a newline when the stream is mid-line.
// The check and both writes happen under one lock: checking outside it would race
// the very thing it guards, and a prose write landing between the newline and p
// would put the break in the wrong place.
func (s *SyncWriter) writeOnItsOwnLine(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.atLineStart {
		if _, err := s.write([]byte{'\n'}); err != nil {
			return 0, err
		}
	}
	return s.write(p)
}

// Emitter writes events as newline-delimited JSON. A zero writer disables it,
// so every call site can emit unconditionally instead of guarding.
type Emitter struct {
	w   *SyncWriter
	now func() time.Time
}

// New builds an emitter writing to w. A *SyncWriter is adopted as it is rather
// than wrapped again, which is how the caller's prose writes and the events end
// up under one lock instead of two.
func New(w io.Writer) *Emitter {
	if w == nil {
		return &Emitter{now: time.Now}
	}
	sw, ok := w.(*SyncWriter)
	if !ok {
		sw = NewSyncWriter(w)
	}
	return &Emitter{w: sw, now: time.Now}
}

// Emit writes one event. It is safe on a nil receiver and on a disabled
// emitter, and safe to call from several goroutines: translation runs a worker
// pool whose progress callbacks arrive concurrently, the SIGINT handler emits
// from a goroutine of its own, and interleaved writes would produce lines no
// consumer could parse.
func (e *Emitter) Emit(ev Event) {
	if e == nil || e.w == nil {
		return
	}
	ev.setBase(ev.kind(), e.now())

	line, err := json.Marshal(ev)
	if err != nil {
		// An event that cannot be encoded is a programming error, not a
		// condition the run should die of: the human output still carries
		// everything a person needs.
		return
	}

	// One write of one whole line, on a line of its own: the lock lives in
	// SyncWriter, so a prose write sharing the same writer is serialised against
	// this one and an unterminated one — a prompt, a carriage-return progress
	// fragment — is broken off first. Marshal above stays outside the lock.
	e.w.writeOnItsOwnLine(append(line, '\n'))
}
