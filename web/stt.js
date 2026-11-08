/* Voice input: the mic buttons in the prompt box and the To do answer box.
 *
 * Tap the mic, speak, tap again (or wait 30 s): the clip is transcribed in
 * a Web Worker (stt-worker.js, Whisper base.en) on this device, and the text
 * goes in at the cursor. It is never sent: you read it, then press Send.
 * No audio leaves the browser, and nothing is downloaded until the first tap
 * and an explicit "Enable voice input".
 *
 * A classic script loaded after app.js, whose helpers it uses (insertPlain,
 * todos, toast ...). app.js builds the buttons with sttMicHtml() and
 * sttStatusHtml(), and calls sttSync() after it repaints them.
 */
"use strict";

(() => {
  const MAX_SECONDS = 30; // Whisper's window
  const MIN_SAMPLES = 16000 * 0.4; // shorter than this is a mis-tap
  const READY_KEY = "comebye.stt.ready"; // "<version>:<backend>" once the user enabled it and it loaded
  const SHORTCUT = "Ctrl+Shift+Space";

  const ICON = {
    mic: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="9" y="3" width="6" height="11" rx="3"/><path d="M5 11a7 7 0 0 0 14 0M12 18v3"/></svg>',
    stop: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="7" y="7" width="10" height="10" rx="1.5" fill="currentColor"/></svg>',
    busy: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3a9 9 0 1 0 9 9"/></svg>',
    down: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 4v11m-4-4 4 4 4-4M5 20h14"/></svg>',
    err: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 8v5m0 3.5v.5"/><circle cx="12" cy="12" r="9"/></svg>',
  };

  const BACKEND_NAME = { webgpu: "GPU", wasm: "CPU (slower)" };
  const fmtMB = (n) => Math.max(1, Math.round(n / 1e6)) + " MB";
  const clock = (s) => Math.floor(s / 60) + ":" + String(Math.floor(s % 60)).padStart(2, "0");
  const esc2 = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);

  const lsGet = (k) => { try { return localStorage.getItem(k); } catch { return null; } };
  const lsSet = (k, v) => { try { localStorage.setItem(k, v); } catch {} };

  // ---- what the UI shows ----
  // kind: idle | checking | missing | offer | listening | downloading |
  //       transcribing | error | note
  const ui = { kind: "idle", target: null, text: "", copy: "", started: 0 };
  let noteTimer = null;

  // ---- the engine (the worker) ----
  const eng = { worker: null, backend: null, probed: null, ready: null, info: null, progress: 0, total: 0, pending: new Map(), seq: 0, force: null };

  // ---- one recording ----
  let rec = null; // { stream, recorder, chunks, tick, stopAt, target, anchor, cancelled }

  function setUi(next) {
    clearTimeout(noteTimer);
    Object.assign(ui, { kind: "idle", target: null, text: "", html: "", copy: "" }, next);
    if (ui.kind === "note") noteTimer = setTimeout(() => { if (ui.kind === "note") setUi({}); }, 6000);
    sync();
  }

  // ---- markup ----

  window.sttMicHtml = (target) =>
    `<button type="button" class="stt-mic" data-stt="${esc2(target)}" data-state="idle" aria-label="Voice input" title="Voice input (${SHORTCUT})">${ICON.mic}</button>`;
  window.sttStatusHtml = (target) =>
    `<div class="stt-status" data-stt-status="${esc2(target)}" role="status" aria-live="polite" hidden></div>`;

  function buttonState(target) {
    if (ui.target !== target) return "idle";
    return { listening: "listening", transcribing: "busy", checking: "busy", downloading: "down", error: "error", missing: "error", offer: "down" }[ui.kind] || "idle";
  }

  const LABEL = {
    idle: "Voice input", listening: "Stop recording", busy: "Voice input is working", down: "Voice input is downloading", error: "Voice input: tap to try again",
  };

  function statusHtml() {
    const dyn = '<span class="stt-dyn"></span>';
    const act = (a, label, cls = "") => `<button type="button" class="stt-act ${cls}" data-act="${a}">${label}</button>`;
    switch (ui.kind) {
      case "checking": return `<span class="stt-msg">Checking voice input…</span>`;
      case "missing": return `<span class="stt-msg">${ui.html}</span>${act("dismiss", "OK")}`;
      case "offer": return `<span class="stt-msg">${esc2(ui.text)}</span>${act("enable", `Enable voice input (downloads ${fmtMB(ui.bytes)})`, "primary")}${act("dismiss", "Not now")}`;
      case "listening": return `<span class="stt-msg">Listening… ${dyn}</span><span class="stt-hint">Tap the mic to stop · Esc cancels</span>`;
      case "downloading": return `<span class="stt-msg">${esc2(ui.text)} ${dyn}</span><progress class="stt-bar" max="1" value="0"></progress>${act("cancel", "Cancel")}`;
      case "transcribing": return `<span class="stt-msg">Transcribing…</span>`;
      case "error": return `<span class="stt-msg">${esc2(ui.text)}</span>${ui.copy ? act("copy", "Copy text") : ""}${act("dismiss", "Dismiss")}`;
      case "note": return `<span class="stt-msg">${esc2(ui.text)}</span>${ui.copy ? act("copy", "Copy text") : ""}`;
    }
    return "";
  }

  // sttSync paints the current state onto every mic button and status line
  // in the page (the same To do item can be drawn twice, phone card and
  // desktop page). app.js calls it after it repaints the To do list.
  function sync() {
    const html = statusHtml();
    const key = ui.kind + "|" + ui.text + "|" + ui.html + "|" + ui.copy + "|" + (ui.bytes || "");
    for (const b of document.querySelectorAll(".stt-mic")) {
      const st = buttonState(b.dataset.stt);
      if (b.dataset.state !== st) {
        b.dataset.state = st;
        b.innerHTML = { listening: ICON.stop, busy: ICON.busy, down: ICON.down, error: ICON.err }[st] || ICON.mic;
      }
      b.setAttribute("aria-label", LABEL[st]);
      b.setAttribute("aria-pressed", st === "listening" ? "true" : "false");
      b.title = st === "listening" ? `Stop (${SHORTCUT})` : `Voice input (${SHORTCUT})`;
      // One thing at a time: the other boxes' mics wait while this one is busy.
      const other = ui.target && ui.target !== b.dataset.stt && ["listening", "transcribing", "downloading"].includes(ui.kind);
      b.toggleAttribute("aria-disabled", !!other);
      b.classList.toggle("stt-wait", !!other);
    }
    for (const s of document.querySelectorAll(".stt-status")) {
      const mine = ui.target === s.dataset.sttStatus && ui.kind !== "idle";
      if (!mine) {
        if (!s.hidden) { s.hidden = true; s.innerHTML = ""; delete s.dataset.key; }
        continue;
      }
      if (s.dataset.key !== key) {
        s.innerHTML = html;
        s.dataset.key = key;
      }
      s.hidden = false;
      s.dataset.kind = ui.kind;
    }
    dyn();
  }
  window.sttSync = sync;

  // dyn updates the parts that change many times a second without rebuilding the status line.
  function dyn() {
    let t = "";
    if (ui.kind === "listening" && rec) {
      const s = Math.min(MAX_SECONDS, (performance.now() - rec.t0) / 1000);
      t = `${clock(s)} / ${clock(MAX_SECONDS)}`;
      if (eng.ready && !eng.isReady && eng.total) t += ` · loading the voice model ${Math.round((100 * eng.progress) / eng.total)}%`;
    } else if (ui.kind === "downloading") {
      t = eng.total ? `${fmtMB(eng.progress)} / ${fmtMB(eng.total)}` : "";
      if (eng.backend) t += ` · ${BACKEND_NAME[eng.backend]}`;
    }
    for (const s of document.querySelectorAll(".stt-status:not([hidden])")) {
      const d = s.querySelector(".stt-dyn");
      if (d && d.textContent !== t) d.textContent = t;
      const bar = s.querySelector(".stt-bar");
      if (bar) bar.value = eng.total ? Math.min(1, eng.progress / eng.total) : 0;
    }
  }

  // ---- the worker ----

  function startWorker() {
    if (eng.worker) return eng.worker;
    const w = new Worker("stt-worker.js", { type: "module" });
    w.onmessage = (ev) => onWorker(ev.data || {});
    w.onerror = (ev) => {
      const msg = (ev && ev.message) || "the voice worker could not start";
      failEngine(new Error(msg));
    };
    eng.worker = w;
    return w;
  }

  function stopWorker() {
    if (eng.worker) eng.worker.terminate();
    eng.worker = null;
    eng.ready = null;
    eng.isReady = false;
    eng.probed = null;
    eng.progress = 0;
    eng.total = 0;
    const err = new Error("voice input was reset");
    for (const p of eng.pending.values()) p.reject(err);
    eng.pending.clear();
    eng.waiters = [];
  }

  // Worker messages settle the promises the callers wait on.
  eng.waiters = [];
  function wait(type) {
    return new Promise((resolve, reject) => eng.waiters.push({ type, resolve, reject }));
  }
  function failEngine(err) {
    const ws = eng.waiters;
    eng.waiters = [];
    for (const w of ws) w.reject(err);
    for (const p of eng.pending.values()) p.reject(err);
    eng.pending.clear();
  }
  function onWorker(m) {
    if (m.type === "progress") {
      eng.progress = m.loaded;
      eng.total = m.total;
      dyn();
      return;
    }
    if (m.type === "error") {
      const err = new Error(m.message);
      err.stage = m.stage;
      if (m.id != null && eng.pending.has(m.id)) {
        eng.pending.get(m.id).reject(err);
        eng.pending.delete(m.id);
      } else {
        const ws = eng.waiters;
        eng.waiters = [];
        for (const w of ws) w.reject(err);
      }
      return;
    }
    if (m.type === "result") {
      const p = eng.pending.get(m.id);
      if (p) { eng.pending.delete(m.id); p.resolve(m); }
      return;
    }
    const keep = [];
    for (const w of eng.waiters) {
      if (w.type === m.type) w.resolve(m);
      else keep.push(w);
    }
    eng.waiters = keep;
  }

  // probe: which backend this browser can run. Asks the worker once.
  function probe() {
    if (eng.probed) return eng.probed;
    const w = startWorker();
    eng.probed = wait("probed").then((m) => { eng.adapter = m.adapter; return eng.force || m.backend; });
    w.postMessage({ type: "probe" });
    return eng.probed;
  }

  // load starts (or joins) the download / cache read and model start-up.
  function load(info, backend) {
    if (eng.ready) return eng.ready;
    eng.backend = backend;
    eng.isReady = false;
    eng.progress = 0;
    eng.total = info.bytes[backend];
    const w = startWorker();
    const p = wait("ready").then((m) => {
      eng.isReady = true;
      lsSet(READY_KEY, `${info.version}:${backend}`);
      return m;
    });
    w.postMessage({ type: "load", base: info.base, backend, bytes: eng.total, version: info.version });
    eng.ready = p;
    p.catch(() => { if (eng.ready === p) eng.ready = null; });
    return p;
  }

  function transcribe(audio) {
    const id = ++eng.seq;
    return new Promise((resolve, reject) => {
      eng.pending.set(id, { resolve, reject });
      eng.worker.postMessage({ type: "transcribe", id, audio }, [audio.buffer]);
    });
  }

  // ---- the host's side: is it installed? ----

  async function hostInfo() {
    if (eng.info && eng.info.installed) return eng.info;
    const res = await fetch("/api/stt", { headers: { Accept: "application/json" } });
    if (res.status === 401 || res.status === 403) throw Object.assign(new Error("not authorized"), { code: "auth" });
    if (!res.ok) throw new Error("the server answered " + res.status);
    const info = await res.json();
    eng.info = info;
    return info;
  }

  // ---- flow ----

  const ERR = {
    insecure: "The microphone only works on a secure page. Open comebye through its secure (https) address, or localhost.",
    unsupported: "This browser can't do voice input here (it needs Web Workers and MediaRecorder).",
    denied: "Microphone blocked. Allow it in the browser's address bar (the lock or mic icon), then tap the mic again.",
    nodevice: "No microphone found.",
    inuse: "The microphone is in use by another app.",
  };

  function micError(e) {
    const n = (e && e.name) || "";
    if (n === "NotAllowedError" || n === "SecurityError") return ERR.denied;
    if (n === "NotFoundError" || n === "OverconstrainedError") return ERR.nodevice;
    if (n === "NotReadableError" || n === "AbortError") return ERR.inuse;
    return "Could not use the microphone: " + ((e && e.message) || n || "unknown error");
  }

  function supported() {
    return !!(window.Worker && window.MediaRecorder && navigator.mediaDevices && navigator.mediaDevices.getUserMedia);
  }

  // Remember where the text goes before focus can move: the caret in the
  // prompt box, or the selection in the answer box.
  function anchorFor(target) {
    if (target === "prompt") {
      const sel = window.getSelection && window.getSelection();
      const inBox = typeof selectionInComposer === "function" && selectionInComposer();
      return { agent: state.selected, range: inBox ? sel.getRangeAt(0).cloneRange() : null };
    }
    const ta = textareaFor(target);
    return { start: ta ? ta.selectionStart : null, end: ta ? ta.selectionEnd : null };
  }

  function textareaFor(target) {
    const id = target.slice("todo:".length);
    const list = typeof todoListEl === "function" ? todoListEl() : null;
    const item = list && list.querySelector(`.td-item[data-id="${CSS.escape(id)}"]`);
    return item ? item.querySelector(".td-text") : null;
  }

  // onMic is every way in: a tap, or the shortcut.
  async function onMic(target) {
    if (rec) { stopRecording(); return; }
    if (["checking", "transcribing", "downloading"].includes(ui.kind)) return;
    if (ui.kind === "offer" && ui.target === target) return;
    const anchor = anchorFor(target);
    if (!window.isSecureContext || !navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
      setUi({ kind: "error", target, text: ERR.insecure });
      return;
    }
    if (!supported()) {
      setUi({ kind: "error", target, text: ERR.unsupported });
      return;
    }
    setUi({ kind: "checking", target });
    let info;
    try {
      info = await hostInfo();
    } catch (e) {
      setUi({ kind: "error", target, text: e.code === "auth" ? "Not authorized to use voice input." : "Could not reach the server: " + e.message });
      return;
    }
    if (!info.installed) {
      setUi({ kind: "missing", target, text: "not installed", html: "Voice input isn't installed on this host. On the host, run <code>comebye stt fetch</code> (it downloads the speech model once, checked against pinned checksums)." });
      return;
    }
    // The microphone first: a refusal should show before a big download.
    let stream;
    try {
      stream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true } });
    } catch (e) {
      setUi({ kind: "error", target, text: micError(e) });
      return;
    }
    let backend;
    try {
      backend = await probe();
    } catch (e) {
      stream.getTracks().forEach((t) => t.stop());
      setUi({ kind: "error", target, text: "Voice input could not start: " + e.message });
      stopWorker();
      return;
    }
    const enabled = lsGet(READY_KEY) === `${info.version}:${backend}`;
    if (!enabled && !(eng.isReady)) {
      // First time (or a different backend): ask before downloading anything.
      stream.getTracks().forEach((t) => t.stop());
      eng.offered = { target, info, backend };
      setUi({
        kind: "offer", target, bytes: info.bytes[backend],
        text: `Voice input runs on this device (${BACKEND_NAME[backend]}); no audio leaves it.${eng.fellBack ? " The GPU could not be used, so this is the slower CPU version." : ""}`,
      });
      return;
    }
    // Start listening at once; the model loads (or comes out of the cache) meanwhile.
    load(info, backend).catch(() => {});
    startRecording(target, stream, anchor, info, backend);
  }

  async function enable() {
    const o = eng.offered;
    if (!o) return;
    eng.offered = null;
    setUi({ kind: "downloading", target: o.target, text: eng.isReady ? "Starting the voice model…" : "Downloading the voice model" });
    try {
      await load(o.info, o.backend);
      setUi({ kind: "note", target: o.target, text: "Voice input is ready. Tap the mic and talk; the text goes in at your cursor and is not sent." });
    } catch (e) {
      await loadFailed(o.target, o.backend, e);
    }
  }

  // A failed load: from the GPU, offer the CPU version (a different, smaller
  // download); otherwise say so and let a tap retry.
  async function loadFailed(target, backend, e) {
    stopWorker();
    if (backend === "webgpu" && !eng.force) {
      eng.force = "wasm";
      eng.fellBack = true;
      try {
        const info = await hostInfo();
        eng.offered = { target, info, backend: "wasm" };
        eng.backend = "wasm";
        setUi({ kind: "offer", target, bytes: info.bytes.wasm,
          text: `The GPU could not start the voice model (${e.message}). The slower CPU version works on any device.` });
        return;
      } catch {}
    }
    setUi({ kind: "error", target, text: `The voice model failed to load: ${e.message}. Tap the mic to try again.` });
  }

  function startRecording(target, stream, anchor, info, backend) {
    let recorder;
    try {
      recorder = new MediaRecorder(stream);
    } catch (e) {
      stream.getTracks().forEach((t) => t.stop());
      setUi({ kind: "error", target, text: ERR.unsupported });
      return;
    }
    const r = { stream, recorder, chunks: [], t0: performance.now(), target, anchor, info, backend, cancelled: false };
    recorder.ondataavailable = (ev) => { if (ev.data && ev.data.size) r.chunks.push(ev.data); };
    recorder.onstop = () => finishRecording(r);
    recorder.onerror = () => { r.cancelled = true; };
    r.tick = setInterval(() => {
      dyn();
      if (performance.now() - r.t0 >= MAX_SECONDS * 1000) { r.hitLimit = true; stopRecording(); }
    }, 200);
    rec = r;
    recorder.start();
    setUi({ kind: "listening", target });
  }

  function stopRecording(cancel = false) {
    if (!rec) return;
    rec.cancelled = rec.cancelled || cancel;
    clearInterval(rec.tick);
    try { if (rec.recorder.state !== "inactive") rec.recorder.stop(); else finishRecording(rec); } catch { finishRecording(rec); }
  }

  async function decode(blob) {
    const Ctx = window.OfflineAudioContext || window.webkitOfflineAudioContext;
    const ab = await new Ctx(1, 16000, 16000).decodeAudioData(await blob.arrayBuffer());
    const n = ab.numberOfChannels;
    if (n === 1) return ab.getChannelData(0).slice();
    const out = new Float32Array(ab.length);
    for (let c = 0; c < n; c++) {
      const d = ab.getChannelData(c);
      for (let i = 0; i < out.length; i++) out[i] += d[i] / n;
    }
    return out;
  }

  async function finishRecording(r) {
    if (rec !== r) return;
    rec = null;
    clearInterval(r.tick);
    r.stream.getTracks().forEach((t) => t.stop());
    if (r.cancelled) { setUi({ kind: "note", target: r.target, text: "Cancelled. Nothing was recorded." }); return; }
    setUi({ kind: "transcribing", target: r.target });
    let audio;
    try {
      audio = await decode(new Blob(r.chunks, { type: r.recorder.mimeType || "audio/webm" }));
    } catch (e) {
      setUi({ kind: "error", target: r.target, text: "Could not read the recording: " + e.message });
      return;
    }
    if (audio.length < MIN_SAMPLES) { setUi({ kind: "note", target: r.target, text: "That was too short to hear. Tap the mic and try again." }); return; }
    // The model may still be loading (the download is the slow part the first time).
    try {
      if (!eng.isReady) {
        setUi({ kind: "downloading", target: r.target, text: eng.total ? "Loading the voice model" : "Starting the voice model" });
        await load(r.info, r.backend);
        setUi({ kind: "transcribing", target: r.target });
      }
    } catch (e) {
      await loadFailed(r.target, r.backend, e);
      return;
    }
    let out;
    try {
      out = await transcribe(audio);
    } catch (e) {
      setUi({ kind: "error", target: r.target, text: "Transcription failed: " + e.message });
      return;
    }
    const text = (out.text || "").trim();
    if (!text) { setUi({ kind: "note", target: r.target, text: "I didn't catch any words. Tap the mic and try again." }); return; }
    insert(r.target, r.anchor, text, r.hitLimit);
  }

  // ---- inserting the text: at the cursor, never sent ----

  function insert(target, anchor, text, hitLimit) {
    const tail = hitLimit ? ` It stopped at ${MAX_SECONDS} s, the longest clip.` : "";
    const done = `Added to the box. Check it, then press ${target === "prompt" ? "Send" : "Answer"}: nothing is sent for you.${tail}`;
    if (target === "prompt") {
      if (state.selected !== anchor.agent) {
        setUi({ kind: "error", target, text: "You switched to another chat while that was being typed up, so it was not added anywhere." + tail, copy: text });
        return;
      }
      const box = document.getElementById("prompt-text");
      if (anchor.range && anchor.range.startContainer.isConnected) {
        box.focus();
        const sel = window.getSelection();
        sel.removeAllRanges();
        sel.addRange(anchor.range);
      }
      const before = typeof textBeforeCaret === "function" ? textBeforeCaret() : "";
      insertPlain((before && !/\s$/.test(before) ? " " : "") + text);
      setUi({ kind: "note", target, text: done });
      return;
    }
    const id = target.slice("todo:".length);
    const ta = textareaFor(target);
    if (ta && !ta.disabled) {
      let s = anchor.start == null ? ta.value.length : Math.min(anchor.start, ta.value.length);
      let e = anchor.end == null ? s : Math.min(anchor.end, ta.value.length);
      let t = text;
      if (s > 0 && !/\s$/.test(ta.value.slice(0, s))) t = " " + t;
      if (ta.maxLength > 0) t = t.slice(0, Math.max(0, ta.maxLength - (ta.value.length - (e - s))));
      ta.setRangeText(t, s, e, "end");
      ta.dispatchEvent(new Event("input", { bubbles: true })); // keeps it as the item's draft
      ta.focus({ preventScroll: true });
      setUi({ kind: "note", target, text: done });
      return;
    }
    // The box was closed meanwhile: keep the words in the item's draft if it still exists.
    const item = typeof todos !== "undefined" && todos.items && todos.items.find((x) => x.id === id && x.state === "open");
    if (item) {
      const old = todos.drafts.get(id);
      todos.drafts.set(id, { text: ((old && old.text) ? old.text.replace(/\s*$/, "") + " " : "") + text, q: item.question });
      if (typeof saveTodoDrafts === "function") saveTodoDrafts();
      setUi({ kind: "note", target, text: "Added to your draft answer for that item. Open it to check it; nothing is sent for you." + tail });
      return;
    }
    setUi({ kind: "error", target, text: "That item was closed while it was being typed up." + tail, copy: text });
  }

  // ---- events ----

  document.addEventListener("pointerdown", (ev) => {
    // Keep the caret where it is: a tap on the mic must not move focus out of the box.
    if (ev.target.closest && ev.target.closest(".stt-mic")) ev.preventDefault();
  }, true);

  document.addEventListener("click", (ev) => {
    const b = ev.target.closest && ev.target.closest(".stt-mic");
    if (b) {
      ev.preventDefault();
      if (b.getAttribute("aria-disabled") === "true") return;
      onMic(b.dataset.stt);
      return;
    }
    const a = ev.target.closest && ev.target.closest(".stt-act");
    if (!a) return;
    const act = a.dataset.act;
    if (act === "enable") enable();
    else if (act === "dismiss") { eng.offered = null; setUi({}); }
    else if (act === "cancel") { stopWorker(); setUi({ kind: "note", target: ui.target, text: "Download cancelled. You can enable voice input again from the mic." }); }
    else if (act === "copy") {
      const text = ui.copy;
      (navigator.clipboard ? navigator.clipboard.writeText(text) : Promise.reject()).then(
        () => { if (typeof toast === "function") toast("Copied"); },
        () => { if (typeof toast === "function") toast("Could not copy", true); },
      );
    }
  });

  // The shortcut works from the prompt box, an answer box, or a mic button.
  function currentTarget() {
    const a = document.activeElement;
    if (!a) return null;
    if (a.id === "prompt-text") return "prompt";
    if (a.classList && a.classList.contains("stt-mic")) return a.dataset.stt;
    if (a.classList && a.classList.contains("td-text")) {
      const item = a.closest(".td-item");
      return item ? "todo:" + item.dataset.id : null;
    }
    return null;
  }

  document.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape" && rec) {
      ev.preventDefault();
      ev.stopPropagation();
      stopRecording(true);
      return;
    }
    if (ev.code === "Space" && ev.ctrlKey && ev.shiftKey && !ev.altKey && !ev.metaKey && !ev.isComposing) {
      const t = rec ? rec.target : currentTarget();
      if (!t) return;
      ev.preventDefault();
      onMic(t);
    }
  }, true);

  window.addEventListener("pagehide", () => { if (rec) stopRecording(true); });

  document.addEventListener("DOMContentLoaded", sync);
  if (document.readyState !== "loading") sync();
})();
