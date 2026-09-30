package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// cliErrKinds reads the CLI's own errkind.go and returns every kind it
// declares, in declaration order.
//
// The list is read rather than duplicated for the same reason
// TestTheGUIStructsMatchTheCLIsJSONTags reads internal/events/events.go: the
// GUI cannot import the CLI's packages (module_test.go), and a hand-copied
// list of thirteen strings is a list that silently stops being thirteen. If
// the CLI classifies a fourteenth failure, TestEveryErrorKindIsRouted below
// picks it up on the next run and proves the window still has somewhere to
// put it.
func cliErrKinds(t *testing.T) []string {
	t.Helper()

	src, err := os.ReadFile("../errkind.go")
	if err != nil {
		t.Fatalf("reading the CLI's errkind.go: %v", err)
	}

	// `errMissingKey errKind = "missing_key"`, with or without the alignment
	// gofmt puts in a const block.
	re := regexp.MustCompile(`err\w+\s+errKind\s*=\s*"([^"]+)"`)
	matches := re.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("found no `err… errKind = \"…\"` declarations in ../errkind.go — " +
			"has the CLI's file changed shape? this guard needs updating")
	}

	kinds := make([]string, 0, len(matches))
	for _, m := range matches {
		kinds = append(kinds, m[1])
	}
	return kinds
}

// TestRoutesToSettings is the spec's table: the five setup kinds and the
// field each one leads to, and every other kind staying where it is.
func TestRoutesToSettings(t *testing.T) {
	for _, tc := range []struct {
		kind, field string
		want        bool
		why         string
	}{
		{"missing_key", "translate.api-key", true, "no key at all is a setup step"},
		{"invalid_key", "translate.api-key", true, "a key with a typo can never succeed on a Retry"},
		{"whisper_not_found", "whisper.bin", true, "the binary is a setting"},
		{"no_model", "whisper.model", true, "the model is a setting"},
		{"ambiguous_model", "whisper.model", true, "several models found, none chosen"},

		{"rate_limited", "", false, "waited out on the Run screen"},
		{"output_unwritable", "", false, "the window offered the path; the user picks another there"},
		{"no_audio_track", "", false, "a fact about the file, not a setting"},
		{"no_subtitle_track", "", false, "a fact about the file, not a setting"},
		{"probe_failed", "", false, "a fact about the file, not a setting"},
		{"transcript_empty", "", false, "a fact about the file, not a setting"},
		{"cancelled", "", false, "the user asked for it"},
		{"other", "", false, "unclassified: there is no field to focus"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			field, ok := RoutesToSettings(tc.kind)
			if ok != tc.want {
				t.Errorf("RoutesToSettings(%q) ok = %v, want %v — %s", tc.kind, ok, tc.want, tc.why)
			}
			if field != tc.field {
				t.Errorf("RoutesToSettings(%q) field = %q, want %q", tc.kind, field, tc.field)
			}
		})
	}
}

// TestEveryErrorKindIsRouted: the CLI has thirteen kinds and a stream can
// carry any of them, including one added after this window was built. Every
// one must land on a screen with a message on it — the Run screen with the
// CLI's own words when it is not a setup step — rather than leaving the
// window blank.
func TestEveryErrorKindIsRouted(t *testing.T) {
	kinds := cliErrKinds(t)
	if len(kinds) != 13 {
		t.Errorf("the CLI declares %d error kinds, not the 13 the spec's table lists: %v — "+
			"if that is deliberate, this count is the reminder to check the new kind "+
			"belongs on the Run screen and not in settingsFields", len(kinds), kinds)
	}

	// The invented kind is the point of the test: it is what a newer CLI looks
	// like to this window.
	for _, kind := range append(slices.Clone(kinds), "a_kind_this_window_has_never_heard_of") {
		t.Run(kind, func(t *testing.T) {
			route := RouteError(kind, "the CLI's own words")

			switch route.Screen {
			case ScreenSettings:
				if _, ok := RoutesToSettings(kind); !ok {
					t.Fatalf("RouteError(%q) went to Settings but RoutesToSettings says it does not", kind)
				}
			case ScreenRun:
				if !route.Retry {
					t.Errorf("RouteError(%q) stays on Run without a Retry button", kind)
				}
			default:
				t.Fatalf("RouteError(%q).Screen = %q, want %q or %q — "+
					"a kind that lands on no screen leaves the window blank",
					kind, route.Screen, ScreenSettings, ScreenRun)
			}

			if route.Message == "" {
				t.Errorf("RouteError(%q).Message is empty: the window would change nothing "+
					"a user can read", kind)
			}
		})
	}

	// And the invented kind specifically: not a settings field, and not
	// silently swallowed.
	field, ok := RoutesToSettings("a_kind_this_window_has_never_heard_of")
	if ok || field != "" {
		t.Errorf("RoutesToSettings(unknown) = (%q, %v), want (\"\", false)", field, ok)
	}
}

// TestEverySettingsFieldIsARealSettingsKey: a routed error focuses a control
// by config key. A field that names no control switches the screen and
// focuses nothing, which to a user is indistinguishable from the window
// ignoring the error.
func TestEverySettingsFieldIsARealSettingsKey(t *testing.T) {
	for kind, field := range settingsFields {
		if !slices.Contains(SettingKeys, field) {
			t.Errorf("kind %q routes to field %q, which is not in SettingKeys %v — "+
				"the Settings screen has no control to focus", kind, field, SettingKeys)
		}
	}
}

// TestRouteErrorAlwaysHasSomethingToSay: the CLI's protocol does not promise
// a non-empty message, and a screen change with no text on it is the one
// outcome routing must never produce.
func TestRouteErrorAlwaysHasSomethingToSay(t *testing.T) {
	for _, tc := range []struct{ kind, message, wantContains string }{
		{"missing_key", "", "missing_key"},
		{"rate_limited", "   ", "rate_limited"},
		{"", "", "without saying why"},
		{"rate_limited", "  slow down  ", "slow down"},
	} {
		route := RouteError(tc.kind, tc.message)
		if !strings.Contains(route.Message, tc.wantContains) {
			t.Errorf("RouteError(%q, %q).Message = %q, want it to contain %q",
				tc.kind, tc.message, route.Message, tc.wantContains)
		}
	}
	// And the message is trimmed rather than shown with the CLI's whitespace.
	if got := RouteError("rate_limited", "  slow down  ").Message; got != "slow down" {
		t.Errorf("RouteError trimmed message = %q, want %q", got, "slow down")
	}
}

// TestStageWordsNamesEveryStageTheCLIEmits reads the CLI's own five stage
// names out of its sources, so a sixth phase cannot appear with the window
// silently showing the previous one's label.
func TestStageWordsNamesEveryStageTheCLIEmits(t *testing.T) {
	re := regexp.MustCompile(`events\.Stage\{\s*Name:\s*"([^"]+)"`)
	found := map[string]bool{}
	for _, file := range []string{"../main.go", "../ordinary.go", "../progressive.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			found[m[1]] = true
		}
	}
	if len(found) == 0 {
		t.Fatal("found no `events.Stage{Name: \"…\"}` in the CLI's sources — " +
			"has it changed shape? this guard needs updating")
	}

	for name := range found {
		words := StageWords(name)
		if words == "" {
			t.Errorf("StageWords(%q) is empty", name)
		}
		if words == name {
			t.Errorf("StageWords(%q) = %q — the CLI emits this stage and the window "+
				"has no words for it, so the Run screen would show the CLI's own identifier", name, words)
		}
	}

	// A stage this window has never heard of shows as the CLI sent it, and a
	// run with no stage yet still says something.
	if got := StageWords("polish"); got != "polish" {
		t.Errorf("StageWords(unknown) = %q, want it echoed back as %q", got, "polish")
	}
	if got := StageWords(""); got == "" {
		t.Error("StageWords(\"\") is empty: the Run screen would show a blank stage line")
	}
}

// TestClosePromptNamesTheStageAndThePercent: the spec's own example of the
// closing-while-running question is a stage and a percentage, because
// "something is running" is not enough to decide on.
func TestClosePromptNamesTheStageAndThePercent(t *testing.T) {
	got := closePrompt("transcribe", 34)
	for _, want := range []string{"Transcribing", "34%"} {
		if !strings.Contains(got, want) {
			t.Errorf("closePrompt(\"transcribe\", 34) = %q, want it to contain %q", got, want)
		}
	}

	// A run that has not reported a stage yet must not claim 0% of a phase it
	// cannot name.
	noStage := closePrompt("", 0)
	if strings.Contains(noStage, "0%") {
		t.Errorf("closePrompt(\"\", 0) = %q, want no percentage for a run that has "+
			"reported no stage yet", noStage)
	}
	if noStage == "" {
		t.Error("closePrompt(\"\", 0) is empty: the dialog would have no question in it")
	}
}

// --- endedFor --------------------------------------------------------------

// TestEndedForGivesEveryEndingSomewhereToGo drives every shape (*Run).Wait
// can return through endedFor and asserts each one produces either a success
// or a route with a message. A blank Run screen after a twenty-minute job is
// the failure this covers.
func TestEndedForGivesEveryEndingSomewhereToGo(t *testing.T) {
	saved := []Saved{{Path: "/out/movie.es.srt", Watchable: false}}

	for _, tc := range []struct {
		name    string
		outcome *Outcome
		err     error
		check   func(t *testing.T, got RunEnded)
	}{
		{
			name:    "done",
			outcome: &Outcome{Done: &Done{Path: "/out/movie.es.srt"}, ExitCode: 0, Saved: saved},
			check: func(t *testing.T, got RunEnded) {
				if !got.OK {
					t.Error("OK = false for a done event")
				}
				if got.Route != nil {
					t.Errorf("Route = %+v for a done event, want nil", got.Route)
				}
				if got.Path != "/out/movie.es.srt" {
					t.Errorf("Path = %q", got.Path)
				}
				if len(got.Saved) != 1 {
					t.Errorf("Saved = %+v, want the run's one saved file", got.Saved)
				}
			},
		},
		{
			name: "done with a warning",
			outcome: &Outcome{
				Done:     &Done{Path: "/out/movie.es.srt"},
				Saved:    saved,
				Warning:  &Warning{Kind: "blocks_untranslated", Count: 12, Total: 1400},
				ExitCode: 0,
			},
			check: func(t *testing.T, got RunEnded) {
				if got.Warning == nil || got.Warning.Count != 12 {
					t.Errorf("Warning = %+v, want the 12 untranslated blocks carried through — "+
						"a file 12 blocks short must not look identical to one that is not", got.Warning)
				}
			},
		},
		{
			name:    "a setup error",
			outcome: &Outcome{Err: &Error{Kind: "missing_key", Message: "no OpenAI key"}, ExitCode: 1},
			check: func(t *testing.T, got RunEnded) {
				if got.OK {
					t.Error("OK = true for an error event")
				}
				if got.Route == nil {
					t.Fatal("Route = nil for an error event")
				}
				if got.Route.Screen != ScreenSettings || got.Route.Field != "translate.api-key" {
					t.Errorf("Route = %+v, want the Settings screen and the api-key field", got.Route)
				}
			},
		},
		{
			name:    "an unknown kind",
			outcome: &Outcome{Err: &Error{Kind: "brand_new_kind", Message: "something new"}, ExitCode: 1},
			check: func(t *testing.T, got RunEnded) {
				if got.Route == nil || got.Route.Screen != ScreenRun || !got.Route.Retry {
					t.Errorf("Route = %+v, want the Run screen with a Retry button", got.Route)
				}
			},
		},
		{
			name: "cancelled with a partial file",
			outcome: &Outcome{
				Err:      &Error{Kind: "cancelled", Message: "cancelled"},
				ExitCode: 130,
				Saved:    []Saved{{Path: "/out/movie.es.srt", Covers: "0:00:00-0:30:00", Watchable: true}},
			},
			check: func(t *testing.T, got RunEnded) {
				if !got.Cancelled {
					t.Error("Cancelled = false for kind cancelled — the screen would title it a failure")
				}
				if len(got.Saved) != 1 || got.Saved[0].Covers != "0:00:00-0:30:00" {
					t.Errorf("Saved = %+v, want what the partial file covers", got.Saved)
				}
			},
		},
		{
			name: "cancelled in the startup race",
			err:  fmt.Errorf("wrapped: %w", ErrCancelled),
			check: func(t *testing.T, got RunEnded) {
				if !got.Cancelled {
					t.Error("Cancelled = false for ErrCancelled")
				}
				if got.ExitCode == 0 {
					t.Error("ExitCode = 0 for a signalled child: that reads as success")
				}
			},
		},
		{
			name: "a stream that broke its contract",
			err:  errors.New("run: sub-translator emitted both a done event and an error event"),
			check: func(t *testing.T, got RunEnded) {
				if got.Cancelled {
					t.Error("Cancelled = true for a broken stream")
				}
				if got.Route == nil || !strings.Contains(got.Route.Message, "both a done event") {
					t.Errorf("Route = %+v, want the Go-level message verbatim: it is the only "+
						"account of what happened", got.Route)
				}
			},
		},
		{
			name: "no outcome and no error",
			check: func(t *testing.T, got RunEnded) {
				if got.Route == nil || got.Route.Message == "" {
					t.Errorf("Route = %+v, want something to show rather than a blank screen", got.Route)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := endedFor(tc.outcome, tc.err)

			// The invariant every case shares, asserted for all of them
			// rather than case by case.
			if got.OK == (got.Route != nil) {
				t.Fatalf("endedFor gave OK=%v and Route=%+v: exactly one of a success and a "+
					"route must be set, or the Run screen has nothing or two things to draw",
					got.OK, got.Route)
			}
			tc.check(t, got)
		})
	}
}
