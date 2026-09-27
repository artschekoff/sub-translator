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
	Duration  float64  `json:"duration,omitempty"`
	Audio     []Stream `json:"audio"`
	Subtitles []Stream `json:"subtitles"`
}

func (*Probe) kind() Kind { return KindProbe }

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

// Emitter writes events as newline-delimited JSON. A zero writer disables it,
// so every call site can emit unconditionally instead of guarding.
type Emitter struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

func New(w io.Writer) *Emitter {
	return &Emitter{w: w, now: time.Now}
}

// Emit writes one event. It is safe on a nil receiver and on a disabled
// emitter, and safe to call from several goroutines: translation runs a worker
// pool whose progress callbacks arrive concurrently, and interleaved writes
// would produce lines no consumer could parse.
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

	e.mu.Lock()
	defer e.mu.Unlock()
	e.w.Write(append(line, '\n'))
}
