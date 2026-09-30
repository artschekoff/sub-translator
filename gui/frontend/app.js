"use strict";

/*
  app.js draws the screens and nothing else.

  Every question with an answer that could be wrong in a way a user sees is
  asked of Go over the Wails binding: whether fast mode applies
  (App.FastMode -> FastAvailable), which output modes to offer (App.ProbeFilm
  -> OutputModes), and whether the chosen track needs a source language
  (App.SourceLang -> SourceLanguage). All three are table-tested in
  gui/rules_test.go. This project ships no node toolchain, so a rule written
  here would have no harness at all — which is the whole reason they are not
  written here.

  What this file does decide is presentation: how a duration reads, what an
  untagged track is called, which screen is visible.
*/

const $ = (id) => document.getElementById(id);

const SCREENS = ["setup", "drop", "busy", "file", "run", "settings"];

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
    $("setup-message").textContent = String(err);
    show("setup");
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

  const info = await app.Boot();
  if (!info.found) {
    $("setup-message").textContent = info.error;
    show("setup");
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

/* ------------------------------------------------------------------- wiring */

function wire() {
  $("setup-choose").addEventListener("click", async () => {
    const app = await ready();
    const info = await app.ChooseCLI();
    if (info.found) {
      show("drop");
      return;
    }
    $("setup-message").textContent = info.error;
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
    /* The Run screen is the next task's. Until it exists this says so rather
       than doing nothing, and it must not pretend a run began. */
    const error = $("file-error");
    error.textContent = "Starting a run is not wired up yet.";
    error.hidden = false;
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
