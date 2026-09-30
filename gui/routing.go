// routing.go holds the Run screen's decisions: where an error leads, what a
// stage is called in words, and what a finished run amounts to.
//
// They are in Go for the same reason rules.go's are (see its package doc):
// this project ships no node toolchain, so a rule written in app.js has no
// harness at all. The routing rule in particular is one a user feels
// immediately when it is wrong — a missing API key sent to a Retry button
// re-runs a whole transcription that can never succeed — and it is a rule the
// spec states outright, so it is table-tested here and the page only renders
// the answer.
package main

import (
	"errors"
	"fmt"
	"strings"
)

// The two screens an error can land on. They are the ids app.js shows, minus
// the "screen-" prefix it adds, so the page can act on the string without a
// second mapping of its own.
const (
	ScreenSettings = "settings"
	ScreenRun      = "run"
)

// settingsFields is the spec's list, verbatim: the five error kinds that are a
// setup step rather than a failure to report, each mapped to the config key
// whose control the Settings screen focuses.
//
// The values are keys from SettingKeys, not labels, because that is what the
// Settings screen's controls carry (see the data-key attributes in
// index.html) — a label would have to be matched by prose.
// TestEverySettingsFieldIsARealSettingsKey pins that: a field that names no
// control would switch the screen and focus nothing, which looks to a user
// exactly like the window ignoring them.
//
// Why the five and not six: invalid_key is here and rate_limited is not,
// although both are the endpoint refusing. A key with a typo in it is
// corrected in Settings and retrying it can never succeed; a rate limit is
// waited out on the Run screen. Folding them together — which an earlier
// draft of the CLI's own error table did — sent the most likely first-run
// error to a button that re-ran the entire transcription. output_unwritable
// is likewise not here: the window is what offered the save path, so the fix
// is to pick another one on the Run screen with the message still on it, not
// to jump to a form field that had nothing wrong with it.
var settingsFields = map[string]string{
	"missing_key":       "translate.api-key",
	"invalid_key":       "translate.api-key",
	"whisper_not_found": "whisper.bin",
	"no_model":          "whisper.model",
	"ambiguous_model":   "whisper.model",
}

// RoutesToSettings reports whether an error of this kind is a setup step to
// complete on the Settings screen, and if so which setting is at fault.
//
// An unrecognised kind — a kind a newer sub-translator added that this window
// has never heard of — returns ("", false), which is the Run screen. That is
// the safe default in both directions: the Run screen always has the CLI's
// own message to show, whereas Settings would have to guess a field, and
// guessing one would focus a control that has nothing to do with the failure.
func RoutesToSettings(kind string) (field string, ok bool) {
	field, ok = settingsFields[kind]
	return field, ok
}

// ErrorRoute is where one error event leads and what the window says there.
//
// Message is never empty. A kind with no message — which the CLI's protocol
// does not promise but a future version could produce — still gets a sentence
// naming the kind, because the one outcome this must never have is a window
// that changed screens and shows nothing.
type ErrorRoute struct {
	Screen  string `json:"screen"`
	Field   string `json:"field"`
	Message string `json:"message"`
	Retry   bool   `json:"retry"`
}

// RouteError turns one error event into the screen it leads to. The five
// setup kinds go to Settings with the field to focus; every other kind stays
// on the Run screen with the message and a Retry button, per the spec's
// "Errors lead somewhere".
//
// Retry is offered for kind "cancelled" too, which reads oddly beside a
// Cancel button the user just pressed but is the honest answer: the run can
// be started again on exactly the terms it was cancelled on, and the Run
// screen also reports what the partial file covers, so nothing about the
// cancel is presented as a failure. Suppressing the button there would leave
// the one ending a user deliberately caused with no way forward but going
// back to the file list.
func RouteError(kind, message string) ErrorRoute {
	msg := strings.TrimSpace(message)
	if msg == "" {
		if kind == "" {
			msg = "sub-translator failed without saying why."
		} else {
			msg = fmt.Sprintf("sub-translator reported an error of kind %q without a message.", kind)
		}
	}

	if field, ok := RoutesToSettings(kind); ok {
		return ErrorRoute{Screen: ScreenSettings, Field: field, Message: msg}
	}
	return ErrorRoute{Screen: ScreenRun, Message: msg, Retry: true}
}

// stageNames is the CLI's five stage names in the words the window shows.
// Kept here rather than in app.js so the close-while-running prompt (see
// closePrompt) and the Run screen's stage line cannot drift into two
// different vocabularies for the same phase.
var stageNames = map[string]string{
	"extract":      "Extracting the audio",
	"extract-subs": "Extracting the subtitle track",
	"transcribe":   "Transcribing",
	"translate":    "Translating",
	"mux":          "Writing the new video file",
}

// StageWords names a stage for a person. A stage name this window has never
// heard of is shown as the CLI sent it rather than dropped: a sixth phase
// added to the CLI should read as an unpolished label, not as the window
// freezing on the fifth.
func StageWords(name string) string {
	if words, ok := stageNames[name]; ok {
		return words
	}
	if name == "" {
		return "Working"
	}
	return name
}

// closePrompt is what the window asks when it is closed mid-run. The spec's
// own example — "transcription is at 34%, leaving now loses it" — is a stage
// and a percentage, so both come from the last stage and progress events the
// run emitted, and a run that has not reported a stage yet says so rather
// than claiming 0% of nothing.
func closePrompt(stage string, pct int) string {
	if stage == "" {
		return "sub-translator is still running. Closing the window now loses the work."
	}
	return fmt.Sprintf("%s is at %d%%. Closing the window now loses the work.", StageWords(stage), pct)
}

// RunEnded is what the page is told when a run finishes, however it
// finished. Exactly one of OK and Route is meaningful: OK true is a done
// event, and Route non-nil is somewhere to go with a message to show.
//
// Saved is every file the run wrote, not just the one Done names: -mode both
// writes a sidecar and a container and done names only the container, so a
// window that offered "Show in Finder" for Done.Path alone would hide the
// .srt that landed minutes earlier.
//
// Cancelled is separate from Route because a cancel is an ending the user
// asked for: it is routed to the Run screen like any other non-setup kind,
// but the screen titles it "Cancelled" rather than "Failed" and shows what
// the partial file covers.
type RunEnded struct {
	OK        bool        `json:"ok"`
	Cancelled bool        `json:"cancelled"`
	ExitCode  int         `json:"exitCode"`
	Path      string      `json:"path"`
	Covers    string      `json:"covers"`
	Saved     []Saved     `json:"saved"`
	Warning   *Warning    `json:"warning"`
	Route     *ErrorRoute `json:"route"`
}

// endedFor turns what (*Run).Wait returned into the ending the page renders.
//
// Every path through it produces either OK or a Route, so the Run screen
// always has something to show. That includes the two endings that are not
// error events at all: ErrCancelled, which is a cancel that landed before
// the CLI could say anything, and a stream that broke the CLI's own
// guarantees, whose Go-level message is the only account of what happened
// and so is shown verbatim.
func endedFor(outcome *Outcome, err error) RunEnded {
	switch {
	case errors.Is(err, ErrCancelled):
		return RunEnded{
			Cancelled: true,
			// A child killed by a signal has no exit code; os/exec reports
			// -1. Reporting 0 here would read as success, and reporting 130
			// would claim a code the process never returned.
			ExitCode: -1,
			Route: &ErrorRoute{
				Screen:  ScreenRun,
				Message: "Cancelled before sub-translator reported anything.",
				Retry:   true,
			},
		}
	case err != nil:
		return RunEnded{Route: &ErrorRoute{Screen: ScreenRun, Message: err.Error(), Retry: true}}
	case outcome == nil:
		// Wait's contract says this cannot happen: it returns a nil *Outcome
		// only with a non-nil error. The branch exists so that if it ever
		// does, the window says so instead of drawing a blank Run screen.
		return RunEnded{Route: &ErrorRoute{
			Screen:  ScreenRun,
			Message: "sub-translator ended without reporting an outcome and without an error.",
			Retry:   true,
		}}
	}

	ended := RunEnded{
		ExitCode: outcome.ExitCode,
		Saved:    outcome.Saved,
		Warning:  outcome.Warning,
	}
	if outcome.Done != nil {
		ended.OK = true
		ended.Path = outcome.Done.Path
		ended.Covers = outcome.Done.Covers
		return ended
	}

	route := RouteError(outcome.Err.Kind, outcome.Err.Message)
	ended.Cancelled = outcome.Err.Kind == "cancelled"
	ended.Route = &route
	return ended
}
