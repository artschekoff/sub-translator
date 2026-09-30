"use strict";

/*
  app.js draws the screens and nothing else.

  Every question with an answer that could be wrong in a way a user sees is
  asked of Go over the Wails binding: whether fast mode applies
  (App.FastMode -> FastAvailable), which output modes to offer (App.ProbeFilm
  -> OutputModes), whether the chosen track needs a source language
  (App.SourceLang -> SourceLanguage), which flag the chosen track index goes on
  (App.StartRun -> trackIndices), and where an error leads (run:ended's route
  -> RoutesToSettings). All of them are table-tested in gui/rules_test.go,
  gui/runner_test.go and gui/routing_test.go. This project ships no node
  toolchain, so a rule written here would have no harness at all — which is the
  whole reason they are not written here.

  What this file does decide is presentation: how a duration reads, what an
  untagged track is called, what a setting's label says, which screen is
  visible.
*/

const $ = (id) => document.getElementById(id);

/* Five screens, one card, one size. Nothing here ever calls WindowSetSize. */
const SCREENS = ["drop", "busy", "file", "run", "settings"];

function show(name) {
  for (const screen of SCREENS) {
    $("screen-" + screen).hidden = screen !== name;
  }
}

/* The form's state, in the CLI's own vocabulary so nothing has to be
   translated on the way out: source is "audio" or "sub" (-source), track is a
   stream index (-track, or the audio index), mode is -mode, out is -out and ""
   means the CLI names the file itself. */
const form = {
  source: "",
  track: -1,
  from: "",
  to: "",
  mode: "srt",
  fast: false,
  out: "",
};

let film = null;
let fromNeeded = false;

/* Where "Done" on the Settings screen goes back to. Settings is reachable from
   three places — the Drop screen, the File screen and an error that routed here
   — and returning to the Drop screen from all of them would throw away a film
   the user had already chosen a track for. */
let settingsReturn = "drop";

/* Every file the run has reported through `saved`, keyed by path so the -fast
   path's repeated saves for the same growing file update one row instead of
   adding fourteen. */
let savedFiles = new Map();

/* Whether a run is in flight, which is what the Cancel button and the Run
   screen's footer are about. */
let running = false;

/* The stage line as last drawn, so a cancel that fails can put it back rather
   than leaving "Cancelling…" on screen over a run that is still going. */
let lastStageLine = "";

/* Which track the source-language field was last prefilled for, as
   "source:index". Prefilling is a per-track act: it must happen when the
   chosen track changes, and it must not happen on a mode change, which would
   overwrite whatever the user was in the middle of typing. */
let prefilledFor = null;

/* ---------------------------------------------------------------- bindings */

let bound = null;

/* The Wails runtime injects window.go.main.App before the page's own scripts
   in the ordinary case, but it is set up from a separate injected script and
   this one must not assume it won. Polling costs nothing and removes a class
   of startup flake that only ever shows on a slow machine. */
function ready() {
  if (bound) return Promise.resolve(bound);
  return new Promise((resolve, reject) => {
    const started = Date.now();
    (function poll() {
      const app = window.go && window.go.main && window.go.main.App;
      if (app) {
        bound = app;
        resolve(app);
        return;
      }
      if (Date.now() - started > 5000) {
        reject(new Error("the window's Go bindings never arrived"));
        return;
      }
      setTimeout(poll, 25);
    })();
  });
}

/* ------------------------------------------------------------ presentation */

function baseName(path) {
  const parts = String(path).split(/[\\/]/);
  return parts[parts.length - 1] || path;
}

/* H:MM:SS, the way a player writes it. A duration the probe could not read is
   omitted rather than printed as 0:00:00 — the CLI omits the field for exactly
   that reason. */
function hms(seconds) {
  if (!(seconds > 0)) return "";
  const total = Math.round(seconds);
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const pad = (n) => String(n).padStart(2, "0");
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}

/* Untagged tracks are common in the wild — the CLI's own track listing marks
   them rather than printing a blank column, and so does this. */
function langLabel(lang) {
  return lang ? lang : "untagged";
}

function titleLabel(stream) {
  return stream.title ? stream.title : "";
}

/* ------------------------------------------------------------------- drops */

function noteDrop(message, isError) {
  const note = $("drop-note");
  note.textContent = message || "";
  note.hidden = !message;
  note.classList.toggle("error", Boolean(isError));
}

/* ------------------------------------------------------------- the screens */

async function boot() {
  let app;
  try {
    app = await ready();
  } catch (err) {
    /* Nothing can be asked of Go, so the only screen that can say anything is
       the static one, and Settings is where a setup problem belongs. Its
       controls all need the bindings, so none of them is offered: an enabled
       Done that leads to a Drop screen which cannot probe anything would be a
       worse answer than one dead end with the reason on it. */
    noteSettings(String(err));
    $("cli-path").textContent = "Unknown";
    $("settings-done").disabled = true;
    show("settings");
    return;
  }

  /* Registered only once the bindings are up, because window.runtime and the
     bindings arrive from the same injected runtime script and this page must
     not assume it ran first.

     This is the only reliable notice that a drag ended. DisableWebViewDrop
     stops WebKit navigating to the dropped film, and it stops the DOM drop
     event with it, so the highlight is cleared here — first, before anything
     that might return early. Go emits on every drop, including one that
     carried no file at all, for exactly this reason. */
  window.runtime.EventsOn("film:dropped", (dropped) => {
    document.body.classList.remove("dragging");
    if (!dropped.path) {
      noteDrop("That drop carried no file.", true);
      show("drop");
      return;
    }
    loadFilm(dropped.path, dropped.ignored);
  });

  wireRunEvents();

  const info = await app.Boot();
  if (!info.found) {
    /* The spec: failing to find the CLI "opens on its settings screen saying
       it cannot find the tool, with a file picker — the same treatment as a
       missing key, and for the same reason: it is a setup step, not a run
       failure". So the same screen, not one of its own. */
    settingsReturn = "drop";
    await openSettings(info.error);
    return;
  }
  show("drop");
}

async function loadFilm(path, ignored) {
  if (!path) return;

  const app = await ready();
  document.body.classList.remove("dragging");
  noteDrop("", false);
  $("busy-message").textContent = "Reading " + baseName(path) + "…";
  show("busy");

  let info;
  try {
    info = await app.ProbeFilm(path);
  } catch (err) {
    /* The CLI's own message, verbatim: for a file ffprobe cannot read it is
       the error event's message, and the window has no better wording for it
       than the tool that looked. */
    film = null;
    noteDrop(String(err), true);
    show("drop");
    return;
  }

  film = info;
  renderFile(ignored);
  show("file");
}

function renderFile(ignored) {
  $("film-name").textContent = film.name;

  const meta = [hms(film.duration), film.container].filter(Boolean);
  if (ignored > 0) {
    /* More than one file was dropped. The first is the one being used, and the
       window says so: silently ignoring the rest is how someone ends up
       translating the wrong film. */
    meta.push(
      ignored === 1
        ? "1 other dropped file ignored"
        : `${ignored} other dropped files ignored`
    );
  }
  $("film-meta").textContent = meta.join(" · ");

  form.source = "";
  form.track = -1;
  form.from = "";
  form.to = $("to").value.trim();
  form.fast = false;
  form.out = "";
  form.mode = film.modes.length > 0 ? film.modes[0].value : "srt";
  prefilledFor = null;

  renderTracks();
  renderModes();
  renderOut();

  $("from").value = "";
  $("from-field").hidden = true;
  $("fast").checked = false;
  $("file-error").hidden = true;

  refresh();
}

/* Audio and subtitle tracks are one radio list, not two: picking audio means
   transcription and picking a subtitle track means translating what is already
   there, so they are alternatives rather than two independent choices. */
function renderTracks() {
  const audio = $("audio-tracks");
  const subs = $("subtitle-tracks");
  audio.replaceChildren();
  subs.replaceChildren();

  for (const stream of film.audio) {
    audio.append(trackRow("audio", stream, "transcribe"));
  }
  for (const stream of film.subtitles) {
    subs.append(trackRow("sub", stream, "translate"));
  }

  /* When the file has no subtitle tracks the section says so rather than
     showing an empty box — that is the case transcription exists for. */
  $("subs-none").hidden = film.subtitles.length > 0;
}

function trackRow(source, stream, what) {
  const row = document.createElement("label");
  row.className = "row";

  const radio = document.createElement("input");
  radio.type = "radio";
  radio.name = "track";
  radio.addEventListener("change", () => {
    form.source = source;
    form.track = stream.index;
    refresh();
  });

  const index = document.createElement("span");
  index.className = "idx";
  index.textContent = "#" + stream.index;

  const lang = document.createElement("span");
  lang.className = "lang";
  lang.textContent = langLabel(stream.lang);

  const title = document.createElement("span");
  title.className = "ttl";
  title.textContent = titleLabel(stream) || stream.codec || "";

  const kind = document.createElement("span");
  kind.className = "what";
  kind.textContent = what;

  row.append(radio, index, lang, title, kind);
  return row;
}

/* The set of modes and the note explaining it both come from Go
   (OutputModes): probe.embeds_subtitles has three states and the difference
   between "this container cannot" and "nobody said" is a decision, not a
   rendering choice. */
function renderModes() {
  const select = $("mode");
  select.replaceChildren();

  for (const mode of film.modes) {
    const option = document.createElement("option");
    option.value = mode.value;
    option.textContent = mode.label;
    option.selected = mode.value === form.mode;
    select.append(option);
  }

  const note = $("modes-note");
  note.textContent = film.modesNote || "";
  note.hidden = !film.modesNote;
}

function renderOut() {
  const label = $("out-path");
  if (form.out) {
    label.textContent = form.out;
  } else {
    label.textContent = "Beside the film, named by sub-translator";
  }
  $("out-reset").hidden = !form.out;
}

/* ------------------------------------------------------- the two Go rules */

/* Every refresh carries a token. Two of the three questions are round trips to
   Go, and a radio clicked twice quickly would otherwise let the first answer
   land after the second and leave the checkbox describing the previous
   choice. */
let refreshSeq = 0;

async function refresh() {
  const seq = ++refreshSeq;
  const app = await ready();

  const fast = $("fast");
  const reason = $("fast-reason");

  if (!form.source) {
    /* Nothing is chosen yet, so there is no source or output pair to ask
       about. Saying "pick a track" is the honest answer; asking Go about an
       empty source would get a sentence about a value the user never set. */
    fast.disabled = true;
    fast.checked = false;
    form.fast = false;
    reason.textContent = "Pick a track first.";
    reason.hidden = false;

    $("from-field").hidden = true;
    fromNeeded = false;
    validate();
    return;
  }

  const [fastState, langState] = await Promise.all([
    app.FastMode(form.source, form.mode),
    app.SourceLang(form.source, form.track),
  ]);
  if (seq !== refreshSeq) return;

  fast.disabled = !fastState.available;
  if (!fastState.available) {
    fast.checked = false;
    form.fast = false;
  }
  reason.textContent = fastState.reason;
  reason.hidden = !fastState.reason;

  /* The control the spec's File screen was missing: -source sub requires
     -from, and the CLI never reads it off the chosen track's lang tag, so it
     is required even for a tagged track. The tag prefills the field; it does
     not replace it. */
  fromNeeded = langState.needed;
  $("from-field").hidden = !fromNeeded;

  const chosen = form.source + ":" + form.track;
  if (!fromNeeded) {
    prefilledFor = null;
    form.from = "";
  } else if (prefilledFor !== chosen) {
    /* A new track. Its own tag fills the field, and an untagged track empties
       it — leaving the previous track's language in place would offer a
       plausible wrong answer, which is worse than an empty field that keeps
       Start disabled. */
    prefilledFor = chosen;
    $("from").value = langState.prefill;
    form.from = langState.prefill;
  }

  validate();
}

/* Start is disabled until the run would actually be valid: a track, a target
   language, and — when the chosen track needs one — a source language. */
function validate() {
  const ok =
    Boolean(form.source) &&
    form.track >= 0 &&
    form.to.length > 0 &&
    (!fromNeeded || form.from.length > 0);
  $("start").disabled = !ok;
}

/* --------------------------------------------------------------- Run screen */

/* Registered once the bindings are up, because window.runtime arrives from the
   same injected script. Every one of these is a fact the CLI stated; none of
   them is a decision. */
function wireRunEvents() {
  window.runtime.EventsOn("run:stage", (stage) => {
    /* The words come from Go (StageWords) so this line and the
       closing-while-running question cannot end up with two vocabularies for
       the same phase. The chunk detail is -fast's alone: the ordinary run's
       stage events carry no chunk/of at all. */
    let line = stage.words;
    if (stage.of > 0) {
      line += ` — chunk ${stage.chunk} of ${stage.of}`;
      if (stage.from && stage.to) line += ` (${stage.from}–${stage.to})`;
    }
    lastStageLine = line;
    $("run-stage").textContent = line;
    /* A new stage starts a new progress burst: the CLI's percentages are
       per-stage, so carrying the previous one over would show a bar that
       jumps backwards. */
    setProgress(0);
  });

  window.runtime.EventsOn("run:progress", (progress) => setProgress(progress.pct));

  window.runtime.EventsOn("run:language", (language) => {
    const note = $("run-note");
    note.textContent = language.detected
      ? `whisper heard ${language.detected}.`
      : "";
    note.hidden = !note.textContent;
  });

  window.runtime.EventsOn("run:saved", (saved) => {
    savedFiles.set(saved.path, saved);
    renderSaved();

    /* watchable:true is the -fast path's one distinguishing claim — this file
       is incomplete and already worth opening — and the ordinary path always
       sends false, so this line appears on one path only. Showing it on the
       ordinary path would promise a moment that run never earned. */
    if (saved.watchable) {
      const note = $("run-watchable");
      note.textContent = saved.covers
        ? `You can start watching now — ${baseName(saved.path)} covers ${saved.covers}.`
        : `You can start watching now — ${baseName(saved.path)} is already usable.`;
      note.hidden = false;
    }
  });

  window.runtime.EventsOn("run:warning", (warning) => showWarning(warning));

  window.runtime.EventsOn("run:ended", (ended) => endRun(ended));
}

function setProgress(pct) {
  const clamped = Math.max(0, Math.min(100, Number(pct) || 0));
  $("run-fill").style.width = clamped + "%";
  $("run-bar").setAttribute("aria-valuenow", String(clamped));
  $("run-pct").textContent = clamped + "%";
}

/* One row per file the run wrote, each with its own "Show in Finder". -mode
   both writes a sidecar and a container, `done` names only the container, and
   the sidecar landed minutes before the mux finished — so the paths come from
   `saved`, not from `done`. */
function renderSaved() {
  const box = $("run-files");
  box.replaceChildren();
  for (const saved of savedFiles.values()) {
    box.append(savedRow(saved));
  }
}

function savedRow(saved) {
  const row = document.createElement("div");
  row.className = "filerow";

  const name = document.createElement("span");
  name.className = "name";
  name.textContent = saved.path;
  name.title = saved.path;

  const covers = document.createElement("span");
  covers.className = "covers";
  covers.textContent = saved.covers || "";

  const reveal = document.createElement("button");
  reveal.className = "btn";
  reveal.textContent = "Show in Finder";
  reveal.addEventListener("click", async () => {
    const app = await ready();
    try {
      await app.RevealFile(saved.path);
    } catch (err) {
      showRunError(String(err));
    }
  });

  row.append(name, covers, reveal);
  return row;
}

/* A file twelve blocks short of translated must not look identical to one that
   is not. */
function showWarning(warning) {
  const line = $("run-warning");
  if (!warning || !warning.count) {
    line.hidden = true;
    return;
  }
  const of = warning.total > 0 ? ` of ${warning.total}` : "";
  line.textContent =
    warning.count === 1
      ? `1 subtitle block${of} could not be translated and kept its original text.`
      : `${warning.count} subtitle blocks${of} could not be translated and kept their original text.`;
  line.hidden = false;
}

function showRunError(message) {
  const line = $("run-error");
  line.textContent = message || "";
  line.hidden = !message;
}

async function startRun() {
  /* Go refuses a second run too (StartRun), but a disabled-looking button that
     silently rejects is worse than one that does nothing. */
  if (running) return;

  const app = await ready();

  savedFiles = new Map();
  running = true;

  $("run-title").textContent = film ? film.name : "";
  lastStageLine = "";
  $("run-stage").textContent = "Starting sub-translator…";
  $("run-note").hidden = true;
  $("run-watchable").hidden = true;
  $("run-warning").hidden = true;
  $("run-files").replaceChildren();
  showRunError("");
  setProgress(0);

  $("run-cancel").hidden = false;
  $("run-cancel").disabled = false;
  $("run-back").hidden = true;
  $("run-retry").hidden = true;

  show("run");

  try {
    /* The form's state, in the CLI's own vocabulary. Which of -track and
       -atrack the chosen index goes on is Go's decision (trackIndices), not
       this file's: the two flags are different streams, and sending an audio
       index as -track is a run that transcribes the wrong dub with no error to
       show for it. */
    await app.StartRun({
      source: form.source,
      track: form.track,
      from: form.from,
      to: form.to,
      mode: form.mode,
      out: form.out,
      fast: form.fast,
    });
  } catch (err) {
    /* The child never started, so no event stream will arrive and run:ended
       will never fire. This is the only notice there will be. */
    running = false;
    endRun({
      ok: false,
      route: { screen: "run", message: String(err), retry: true },
    });
  }
}

async function cancelRun() {
  const app = await ready();
  const button = $("run-cancel");
  button.disabled = true;
  $("run-stage").textContent = "Cancelling — waiting for sub-translator to clean up…";

  try {
    await app.CancelRun();
  } catch (err) {
    /* Go's Cancel returns nil for every cancel that worked, including the one
       that landed before the CLI could answer and one pressed on a run that had
       already finished, so a rejection here really is something gone wrong —
       and the run is still going. The button comes back, because the one state
       where someone would want to press it again is the one where it failed,
       and the stage line goes back to saying what is happening rather than
       leaving "Cancelling…" on screen forever. */
    showRunError(String(err));
    button.disabled = false;
    $("run-stage").textContent = lastStageLine || "Still working.";
  }
  /* Nothing is concluded here: the ending still arrives on run:ended, which is
     what reports the partial file and what it covers. */
}

async function endRun(ended) {
  running = false;

  $("run-cancel").hidden = true;
  $("run-back").hidden = false;
  $("run-back").textContent = film ? "Back to the file" : "Choose another file";

  /* The authoritative list, in case a `saved` event was emitted before this
     page had a listener for it. */
  if (ended.saved && ended.saved.length > 0) {
    savedFiles = new Map(ended.saved.map((saved) => [saved.path, saved]));
    renderSaved();
  }
  showWarning(ended.warning);

  if (ended.ok) {
    setProgress(100);
    $("run-stage").textContent = "Done.";
    $("run-watchable").hidden = true;
    showRunError("");
    $("run-retry").hidden = true;
    return;
  }

  /* Go always sends a route with an ending that is not ok (endedFor), so this
     fallback should be unreachable. It exists because the one thing this screen
     must never do after twenty minutes of work is go blank. */
  const route = ended.route || {
    screen: "run",
    message: "sub-translator ended without saying how.",
    retry: true,
  };

  $("run-stage").textContent = ended.cancelled ? "Cancelled." : "Stopped.";
  $("run-watchable").hidden = true;

  if (route.screen === "settings") {
    /* Not a failure to report but a setup step to complete. The field at fault
       is focused and the message sits above it; Retry is deliberately not
       offered, because a key with a typo in it can never succeed on one. */
    settingsReturn = "file";
    await openSettings(route.message, route.field);
    return;
  }

  showRunError(route.message);
  $("run-retry").hidden = !route.retry;
}

/* ---------------------------------------------------------- Settings screen */

/* The labels are presentation, so they live here. The keys and their order are
   Go's (SettingKeys), and the rows are built from what Go sends, so a key added
   there appears here without this file changing — labelled with its own key
   until someone writes it a better one, which is visibly unpolished rather than
   silently missing. */
const SETTING_LABELS = {
  "translate.api-key": "OpenAI API key",
  "translate.model": "Translation model",
  "translate.workers": "Batches translated at once",
  "whisper.bin": "whisper-cli binary",
  "whisper.model": "whisper ggml model",
  "whisper.vad-model": "Silero VAD model",
  "whisper.chunk-minutes": "Chunk length for fast mode, in minutes",
  "models.dir": "Where models are kept",
};

function noteSettings(message) {
  const note = $("settings-message");
  note.textContent = message || "";
  note.hidden = !message;
}

/* openSettings reads every value through the CLI each time the screen opens.
   The window keeps no copy: a value changed in the terminal has to show up
   here, and the other way round, which is the whole reason these go through
   `config get` and `config set` rather than through a file this window also
   parses. */
async function openSettings(message, field) {
  const app = await ready();

  show("settings");
  /* The message goes above the field it is about when there is one — that is
     what makes a screen change explicable — and at the top of the screen only
     when it belongs to the screen rather than to a setting. */
  noteSettings(field ? "" : message);

  let view;
  try {
    view = await app.LoadSettings();
  } catch (err) {
    noteSettings(String(err));
    return;
  }

  $("cli-path").textContent = view.cli || "Not found";
  renderSettings(view);

  const notices = new Set();
  if (message && !field) notices.add(message);
  if (view.error) notices.add(view.error);
  noteSettings([...notices].join(" "));

  if (field) focusSetting(field, message);
}

function renderSettings(view) {
  const box = $("settings-fields");
  box.replaceChildren();

  if (!view.found) {
    /* Every one of the eight is read by running the CLI, so with no CLI there
       is nothing honest to put in the fields. Saying so beats eight empty
       boxes that silently refuse to save. */
    const note = document.createElement("p");
    note.className = "hint";
    note.textContent =
      "sub-translator's own settings are read and written by running it, so " +
      "they cannot be shown until the binary above is found.";
    box.append(note);
    $("settings-done").disabled = true;
    return;
  }

  $("settings-done").disabled = false;
  for (const key of view.keys || []) {
    box.append(settingRow(key, (view.values && view.values[key]) || ""));
  }
}

function settingRow(key, value) {
  const row = document.createElement("div");
  row.className = "setting";
  row.dataset.key = key;

  const label = document.createElement("label");
  label.className = "label";
  label.textContent = SETTING_LABELS[key] || key;
  label.setAttribute("for", "set-" + key);

  const line = document.createElement("div");
  line.className = "line";

  const input = document.createElement("input");
  input.className = "control";
  input.id = "set-" + key;
  input.dataset.key = key;
  input.autocomplete = "off";
  input.spellcheck = false;
  input.value = value;

  const answer = document.createElement("p");
  answer.className = "hint answer";
  answer.hidden = true;

  /* Written on change — which fires on blur and on Enter — rather than on
     every keystroke: each write is one `config set` subprocess, and an API key
     typed character by character would be fifty of them. */
  input.addEventListener("change", async () => {
    const app = await ready();
    row.classList.remove("at-fault");
    try {
      await app.SaveSetting(key, input.value.trim());
      answer.classList.remove("error");
      answer.textContent = "Saved.";
    } catch (err) {
      /* The CLI's own rejection, verbatim. `config set translate.workers -1`
         answers `translate.workers must be a positive integer, got "-1"`, and
         this window has no better wording for a rule it deliberately holds no
         copy of. Four of these keys and the API key validate nothing at all, so
         they simply save. */
      answer.classList.add("error");
      answer.textContent = String(err);
    }
    answer.hidden = false;
  });

  line.append(input);

  /* The API key is masked with a Show toggle beside the field rather than an
     eye glyph inside it: "no web fonts, no icon fonts" rules out the glyph. */
  if (key === "translate.api-key") {
    input.type = "password";
    const reveal = document.createElement("button");
    reveal.className = "btn reveal";
    reveal.textContent = "Show";
    reveal.addEventListener("click", () => {
      const masked = input.type === "password";
      input.type = masked ? "text" : "password";
      reveal.textContent = masked ? "Hide" : "Show";
    });
    line.append(reveal);
  }

  /* The answer sits between the label and the control, not under it. The spec
     says a routed error "shows the message above" the field at fault, and the
     reason is legibility rather than taste: a message under a control reads as
     a caption for whatever comes next, and on this screen what comes next is
     another setting. */
  row.append(label, answer, line);
  return row;
}

/* The other half of "errors lead somewhere": the screen changed, so the reason
   has to be visible at the control that caused it, and the control has to be
   the one the keyboard is already in. */
function focusSetting(key, message) {
  const row = $("settings-fields").querySelector('.setting[data-key="' + key + '"]');
  if (!row) return;

  row.classList.add("at-fault");

  const answer = row.querySelector(".answer");
  if (answer && message) {
    answer.textContent = message;
    answer.classList.add("error");
    answer.hidden = false;
  }

  const input = row.querySelector("input");
  if (input) {
    input.focus();
    input.scrollIntoView({ block: "nearest" });
  }
}

/* ------------------------------------------------------------------- wiring */

function wire() {
  $("drop-settings").addEventListener("click", async () => {
    settingsReturn = "drop";
    await openSettings("");
  });

  $("cli-choose").addEventListener("click", async () => {
    const app = await ready();
    let info;
    try {
      info = await app.ChooseCLI();
    } catch (err) {
      noteSettings(String(err));
      return;
    }
    /* Reopened rather than patched in place: finding the CLI is what makes the
       eight settings below readable at all. */
    await openSettings(info.found ? "" : info.error);
  });

  $("settings-done").addEventListener("click", () => {
    show(settingsReturn === "file" && film ? "file" : "drop");
  });

  $("run-cancel").addEventListener("click", () => cancelRun());
  $("run-retry").addEventListener("click", () => startRun());
  $("run-back").addEventListener("click", () => {
    if (film) {
      show("file");
      return;
    }
    noteDrop("", false);
    show("drop");
  });

  $("drop-choose").addEventListener("click", async () => {
    const app = await ready();
    const path = await app.ChooseFilm();
    if (path) loadFilm(path, 0);
  });

  $("file-back").addEventListener("click", () => {
    film = null;
    noteDrop("", false);
    show("drop");
  });

  $("to").addEventListener("input", (event) => {
    form.to = event.target.value.trim();
    validate();
  });

  $("from").addEventListener("input", (event) => {
    form.from = event.target.value.trim();
    validate();
  });

  $("mode").addEventListener("change", (event) => {
    form.mode = event.target.value;
    refresh();
  });

  $("fast").addEventListener("change", (event) => {
    form.fast = event.target.checked;
  });

  $("out-choose").addEventListener("click", async () => {
    const app = await ready();
    const path = await app.ChooseOutput();
    if (path) {
      form.out = path;
      renderOut();
    }
  });

  $("out-reset").addEventListener("click", () => {
    form.out = "";
    renderOut();
  });

  $("start").addEventListener("click", () => {
    $("file-error").hidden = true;
    startRun();
  });

  /* The drop itself is delivered by Go, from runtime.OnFileDrop: on macOS the
     native view collects the paths and the webview never sees a drop event
     (DisableWebViewDrop in main.go stops WebKit navigating to the film). These
     DOM listeners are only the highlight while a file is over the window. */
  window.addEventListener("dragover", (event) => {
    event.preventDefault();
    document.body.classList.add("dragging");
  });
  window.addEventListener("dragleave", () => {
    document.body.classList.remove("dragging");
  });
  window.addEventListener("drop", (event) => {
    event.preventDefault();
    document.body.classList.remove("dragging");
  });
}

wire();
boot();
