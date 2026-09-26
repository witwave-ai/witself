/* witself dashboard — read-only viewer over the local /api proxy (ADR 0004).
   No frameworks, no external requests; everything the page loads is embedded
   in the witself binary and served same-origin. */
"use strict";

(function () {
  var THEME_KEY = "witself-dashboard-theme";
  var state = {
    self: null,
    eventSource: null,
    sseTranscript: null, // transcript id the current EventSource tails
    sseMessages: false,  // whether the current EventSource polls the mailbox
    sseMemories: false,  // whether the current EventSource polls memories
    sseFacts: false,     // whether the current EventSource polls facts
    sseSecrets: false,   // whether the current EventSource polls secrets
    sseEmail: false,     // whether the current EventSource polls received email metadata
    sseEmailSent: false, // whether the current EventSource polls sent email metadata
    sseEmailUnread: false,
    sseEmailUnacked: false,
    seenSequences: {},   // transcript id -> highest rendered sequence
    messages: {},        // direction + " " + message id -> passive metadata
    emailAddress: null,  // display-only receive address projection
    emailStatus: null,   // value-free raw-message and attachment capacity
    emailMessages: [],   // metadata only; no ids, bodies, MIME, or claim fence
    emailAvailable: null,
    emailReceiveUnavailableReason: null,
    emailReceiveDegraded: false,
    emailReceiveLiveRevision: 0, // invalidates slower direct reads after a live frame
    emailReceiveRequestRevision: 0, // newest direct status/list read wins within one view
    emailViewGeneration: 0, // invalidates direct reads from an earlier visit to the pane
    emailAddressRecoveryPending: false, // one bounded reprobe after a transient initial miss
    emailCheckpointEnabled: null,
    emailFilters: { unread: false, unacked: false },
    emailSentMessages: [], // content-minimal metadata; no ids or submitted text
    emailSentAvailable: null,
    emailSentUnavailableReason: null,
    emailSentDegraded: false,
    emailSentLiveRevision: 0, // invalidates slower direct reads after a live frame
    facts: {},           // fact id -> redacted fact (never a revealed value)
    filters: {},         // section -> list filter text, reapplied on re-render
    upstreamErrors: {},  // SSE source -> upstream error text while degraded
    lastSelfData: null,     // last raw "self" frame, to skip no-op re-renders
    lastMemoriesData: null, // last raw "memories" frame, same reason
    lastFactsData: null,    // last raw "facts" frame, same reason
    lastSecretsData: null,  // last raw "secrets" frame, same reason
    lastEmailData: null,    // received list + capacity frame; policy changes alter it
    lastEmailSentData: null,// sent lifecycle frame, independent of receive policy
    themes: ["console"],    // replaced by /api/themes (the embedded theme dir)
  };

  function $(id) { return document.getElementById(id); }

  function esc(value) {
    return String(value == null ? "" : value)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }

  // --- theme ------------------------------------------------------------
  // Themes come from /api/themes (the embedded static/themes directory), so
  // shipping a new theme is dropping a CSS file there — never a JS or HTML
  // edit (ADR 0004). "auto" is a client-side picker entry (never a file):
  // it resolves to paper when the OS prefers light and console when dark,
  // re-resolving live on scheme changes. Unknown names fall back to the
  // default, and every stylesheet URL still passes the file-backed theme
  // whitelist — a tampered server pref or localStorage value can only select
  // an embedded pack, never become an arbitrary URL.
  var AUTO_THEME = "auto";
  var PREFS_SCHEMA = "witself.dashboard-prefs.v1";
  var prefersLight = window.matchMedia ? window.matchMedia("(prefers-color-scheme: light)") : null;

  function pickerThemes() { return [AUTO_THEME].concat(state.themes); }

  function defaultTheme() {
    return state.themes.indexOf("console") >= 0 ? "console" : state.themes[0];
  }

  function resolveTheme(name) {
    if (name !== AUTO_THEME) { return name; }
    var preferred = prefersLight && prefersLight.matches ? "paper" : "console";
    return state.themes.indexOf(preferred) >= 0 ? preferred : defaultTheme();
  }

  function applyTheme(name, persist) {
    if (pickerThemes().indexOf(name) < 0) { name = defaultTheme(); }
    // The resolver's output goes back through the file-backed whitelist:
    // only an embedded pack name may become a stylesheet URL.
    var resolved = resolveTheme(name);
    if (state.themes.indexOf(resolved) < 0) { resolved = defaultTheme(); }
    $("theme-css").setAttribute("href", "/static/themes/" + encodeURIComponent(resolved) + ".css");
    $("theme-select").value = name;
    try { localStorage.setItem(THEME_KEY, name); } catch (_) { /* private mode */ }
    if (persist === true) { putThemePref(name); }
  }

  // Fire-and-forget persistence: the cell row is the durable copy (it follows
  // the agent across machines and rides account export/import), while
  // localStorage stays the offline fallback when the PUT cannot land.
  function putThemePref(name) {
    fetch("/api/prefs", {
      method: "PUT",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prefs: { schema: PREFS_SCHEMA, theme: name } }),
    }).then(function (resp) {
      if (!resp.ok) { throw new Error("HTTP " + resp.status); }
    }).catch(function (err) {
      toast("theme kept locally; saving to cell failed: " + (err.message || err));
    });
  }

  function initTheme() {
    var fromQuery = new URLSearchParams(window.location.search).get("theme");
    var stored = null;
    try { stored = localStorage.getItem(THEME_KEY); } catch (_) { /* private mode */ }
    Promise.all([
      fetchJSON("/api/themes").catch(function () { return null; }),
      fetchJSON("/api/prefs").catch(function () { return null; }),
    ]).then(function (results) {
      var names = (((results[0] || {}).themes) || []).filter(function (name) {
        return /^[A-Za-z0-9][A-Za-z0-9_.-]*$/.test(name);
      });
      if (names.length) { state.themes = names; }
      var row = (results[1] || {}).preferences;
      var serverTheme = row && row.prefs && typeof row.prefs.theme === "string" ? row.prefs.theme : null;
      $("theme-select").innerHTML = pickerThemes().map(function (name) {
        return '<option value="' + esc(name) + '">' + esc(name) + "</option>";
      }).join("");
      // Precedence: explicit ?theme= > the agent's stored server pref >
      // this browser's localStorage > the default; whatever wins is still
      // validated by applyTheme's whitelist before any stylesheet loads.
      applyTheme(fromQuery || serverTheme || stored || "console");
    });
    $("theme-select").addEventListener("change", function (event) {
      applyTheme(event.target.value, true);
    });
    if (prefersLight) {
      var reResolve = function () {
        if ($("theme-select").value === AUTO_THEME) { applyTheme(AUTO_THEME); }
      };
      if (prefersLight.addEventListener) { prefersLight.addEventListener("change", reResolve); }
      else if (prefersLight.addListener) { prefersLight.addListener(reResolve); }
    }
  }

  // --- portrait ---------------------------------------------------------
  function initAvatarDialog() {
    var trigger = $("avatar-trigger"), dialog = $("avatar-dialog"), close = $("avatar-close"), image = $("avatar-enlarged");
    // Older embedded browsers and lightweight DOM consumers may omit dialogs.
    if (!trigger || !dialog || !close || !image) { return; }
    if (typeof dialog.showModal !== "function" || typeof dialog.close !== "function") {
      trigger.disabled = true;
      return;
    }
    trigger.addEventListener("click", function () {
      if (dialog.open) { return; }
      if (!image.getAttribute("src")) { image.setAttribute("src", "/api/avatar.svg"); }
      dialog.showModal();
      close.focus();
    });
    close.addEventListener("click", function () { dialog.close(); });
    dialog.addEventListener("cancel", function (event) {
      event.preventDefault();
      dialog.close();
    });
    dialog.addEventListener("close", function () { trigger.focus(); });
  }

  // --- data -------------------------------------------------------------
  function fetchJSON(path) {
    return fetch(path, { credentials: "same-origin" }).then(function (resp) {
      if (!resp.ok) {
        return resp.json().catch(function () { return {}; }).then(function (body) {
          var err = new Error(body.error || (path + ": HTTP " + resp.status));
          err.status = resp.status;
          throw err;
        });
      }
      return resp.json();
    });
  }

  // --- list filtering ---------------------------------------------------
  // One filter input per list view, matching case-insensitively against each
  // row's visible text. Filtering is pure client-side over already-fetched
  // rows (zero requests); the value lives in state per section so SSE-driven
  // re-renders reapply it, and clearing the input shows every row again.
  function filterInputHTML(section) {
    return '<input class="filter-input" id="filter-' + esc(section) + '" type="search"' +
      ' placeholder="filter\u2026" aria-label="filter ' + esc(section) + '"' +
      ' value="' + esc(state.filters[section] || "") + '">' +
      '<div class="filter-empty" id="filter-empty-' + esc(section) + '" hidden>' +
      '<span role="status">No matching ' + esc(section) + '.</span> ' +
      '<button type="button" class="clear-filter" id="clear-filter-' + esc(section) + '">Clear filter</button></div>';
  }

  function applyRowFilter(section) {
    var input = $("filter-" + section);
    var panel = input && input.closest ? input.closest(".panel") : null;
    if (!panel) { return; }
    var query = (state.filters[section] || "").toLowerCase();
    var rows = panel.querySelectorAll(".row"), matches = 0;
    rows.forEach(function (row) {
      var visible = !query || row.textContent.toLowerCase().indexOf(query) >= 0;
      row.style.display = visible ? "" : "none";
      if (visible) { matches++; }
    });
    var empty = $("filter-empty-" + section);
    if (empty) { empty.hidden = !query || !rows.length || matches > 0; }
  }

  function bindFilter(section) {
    var input = $("filter-" + section);
    if (!input) { return; }
    input.addEventListener("input", function () {
      state.filters[section] = input.value;
      applyRowFilter(section);
    });
    var clear = $("clear-filter-" + section);
    if (clear) { clear.addEventListener("click", function () {
      input.value = "";
      state.filters[section] = "";
      applyRowFilter(section);
      input.focus();
    }); }
    applyRowFilter(section);
  }

  // Shared route-owned browse/open navigation. Retain only browse metadata and
  // geometry in this page; expanded detail and revealed bodies are never cached.
  var focusLists = Object.create(null);
  var focusRoute = { section: "", id: "" };
  function focusState(section) {
    return focusLists[section] || (focusLists[section] = { selected: "", viewTop: 0, documentTop: 0 });
  }
  function focusRouteChanged(current) {
    var previous = focusRoute;
    if (previous.section && !previous.id && $("focus-inventory")) {
      var saved = focusState(previous.section);
      saved.viewTop = $("view").scrollTop;
      saved.documentTop = document.scrollingElement ? document.scrollingElement.scrollTop : 0;
    }
    if (previous.section !== current.section && focusLists[previous.section]) {
      // Re-entering a section fetches a fresh inventory. Keep only navigation.
      delete focusLists[previous.section].page;
      delete focusLists[previous.section].loaded;
    }
    focusRoute = { section: current.section, id: current.id || "" };
    focusState(current.section).returning = previous.section === current.section && !!previous.id && !current.id;
  }
  function focusControl(section, id, label) {
    return '<a class="focus-open" href="#/' + esc(section) + '/' + esc(encodeURIComponent(id)) +
      '" data-focus-id="' + esc(id) + '" aria-expanded="false" aria-controls="focus-detail">' + esc(label) + '</a>';
  }
  function announceFocus(text) {
    var status = $("focus-status");
    if (status) { status.textContent = text; }
  }
  function focusElement(node) {
    if (node && typeof node.focus === "function") { node.focus({ preventScroll: true }); }
  }
  function renderFocusList(section, html) {
    var saved = focusState(section), view = $("view");
    var caret = captureFilterFocus(section);
    var active = document.activeElement;
    var hadRowFocus = active && active.classList && active.classList.contains("focus-open");
    var viewTop = view.scrollTop;
    var documentTop = document.scrollingElement ? document.scrollingElement.scrollTop : 0;
    view.innerHTML = '<div class="focus-layout"><div id="focus-inventory" class="focus-inventory">' + html +
      '</div><section id="focus-detail" hidden></section></div>';
    bindFilter(section);
    var controls = Array.prototype.slice.call(view.querySelectorAll(".focus-open"));
    function select(control) {
      saved.selected = control.getAttribute("data-focus-id");
      controls.forEach(function (node) {
        var selected = node === control;
        node.setAttribute("aria-current", selected ? "true" : "false");
        node.closest(".row").classList.toggle("selected", selected);
      });
    }
    controls.forEach(function (control) {
      control.addEventListener("focus", function () { select(control); });
      control.addEventListener("click", function () { select(control); });
      control.addEventListener("keydown", function (event) {
        var visible = controls.filter(function (node) { return node.closest(".row").style.display !== "none"; });
        var index = visible.indexOf(control), next;
        if (event.key === "ArrowDown") { next = Math.min(visible.length - 1, index + 1); }
        if (event.key === "ArrowUp") { next = Math.max(0, index - 1); }
        if (event.key === "Home") { next = 0; }
        if (event.key === "End") { next = visible.length - 1; }
        if (next !== undefined && visible[next]) {
          event.preventDefault();
          select(visible[next]);
          visible[next].focus();
        } else if (event.key === " " || event.key === "Enter") {
          event.preventDefault();
          select(control);
          window.location.hash = control.getAttribute("href");
        }
      });
    });
    var selected = controls.filter(function (node) { return node.getAttribute("data-focus-id") === saved.selected; })[0];
    if (selected) { select(selected); }
    if (saved.returning) {
      focusElement(selected && selected.closest(".row").style.display !== "none" ? selected : $("filter-" + section));
      view.scrollTop = saved.viewTop;
      if (document.scrollingElement) { document.scrollingElement.scrollTop = saved.documentTop; }
      saved.returning = false;
      announceFocus(section + " list expanded.");
    } else {
      restoreFilterFocus(section, caret);
      if (hadRowFocus) { focusElement(selected); }
      view.scrollTop = viewTop;
      if (document.scrollingElement) { document.scrollingElement.scrollTop = documentTop; }
    }
  }
  function renderFocusDetail(section, id, label, html, moveFocus) {
    focusState(section).selected = id;
    $("view").innerHTML = '<div class="focus-layout"><div id="focus-inventory" class="focus-inventory" hidden></div>' +
      '<div class="focus-header"><span class="focus-identity">' + esc(label) + '</span>' +
      '<button type="button" class="focus-back" aria-expanded="true" aria-controls="focus-detail">Back</button></div>' +
      '<section id="focus-detail" class="focus-detail" tabindex="-1" aria-label="' + esc(label) + '">' + html + '</section></div>';
    var back = $("view").querySelector(".focus-back");
    function close() { window.location.hash = "#/" + section; }
    back.addEventListener("click", close);
    $("view").querySelector(".focus-layout").addEventListener("keydown", function (event) {
      if (event.key === "Escape") { event.preventDefault(); close(); }
    });
    if (moveFocus !== false) {
      focusElement($("focus-detail"));
      announceFocus(section + " list collapsed. " + label + " expanded. Back returns to the list.");
    }
  }

  // An SSE-driven list re-render replaces the whole panel: the rebuilt input
  // carries the saved filter value but not keyboard focus, so keystrokes
  // landing mid-typing would silently go nowhere. Capture focus and caret
  // before the innerHTML swap. Clear filter is also a keyboard focus target;
  // if new matches remove that control, return focus to the input instead.
  function captureFilterFocus(section) {
    var active = document.activeElement;
    if (active && active.id === "clear-filter-" + section) { return { target: "clear" }; }
    if (!active || active.id !== "filter-" + section) { return null; }
    return { target: "input", start: active.selectionStart, end: active.selectionEnd };
  }

  function restoreFilterFocus(section, caret) {
    if (!caret) { return; }
    var input = $("filter-" + section);
    if (!input) { return; }
    if (caret.target === "clear") {
      var empty = $("filter-empty-" + section), clear = $("clear-filter-" + section);
      if (empty && !empty.hidden && clear) { clear.focus({ preventScroll: true }); return; }
      input.focus({ preventScroll: true });
      return;
    }
    input.focus({ preventScroll: true });
    try { input.setSelectionRange(caret.start, caret.end); } catch (_) { /* unsupported */ }
  }

  function renderHeader(self) {
    state.self = self;
    var identity = self.identity || {};
    $("agent-name").textContent = identity.agent_name || "(unnamed agent)";
    $("realm-name").textContent = identity.realm_name || "";
    $("agent-id").textContent = identity.agent_id || "";
    $("version").textContent = self.dashboard_version ? "v" + self.dashboard_version : "";
    if (self.poll_interval_ms) {
      $("status-poll").textContent = "poll " + (self.poll_interval_ms / 1000) + "s";
    }
    $("status-addr").textContent = window.location.host;
  }

  // The address endpoint supplies the immutable address metadata once when
  // the pane opens. Receive switches are live operational state, so refresh
  // those three value-free fields from every self checkpoint rather than
  // leaving an open pane stale until navigation/reload.
  function updateEmailAddressFromCheckpoint(checkpoint) {
    if (!checkpoint || checkpoint.unavailable) { return false; }
    if (checkpoint.enabled === false) {
      state.emailReceiveLiveRevision++;
      state.emailCheckpointEnabled = false;
      var disabledChanged = state.emailAvailable !== false || state.emailAddress !== null ||
        state.emailStatus !== null ||
        (state.emailMessages && state.emailMessages.length !== 0);
      state.emailAvailable = false;
      state.emailReceiveUnavailableReason = "feature_disabled";
      state.emailReceiveDegraded = false;
      state.emailAddressRecoveryPending = false;
      state.emailAddress = null;
      state.emailStatus = null;
      state.emailMessages = [];
      return disabledChanged ? "disabled" : false;
    }
    var reenabled = checkpoint.enabled === true && state.emailCheckpointEnabled === false;
    if (checkpoint.enabled === true) { state.emailCheckpointEnabled = true; }
    if (reenabled) {
      state.emailReceiveLiveRevision++;
      return "reenabled";
    }
    if (!state.emailAddress) { return false; }
    var changed = false;
    [["receive_state", checkpoint.receive_state],
      ["agent_receive_state", checkpoint.agent_receive_state],
      ["realm_receive_state", checkpoint.realm_receive_state]].forEach(function (pair) {
      if (typeof pair[1] !== "string" || !pair[1] || state.emailAddress[pair[0]] === pair[1]) { return; }
      state.emailAddress[pair[0]] = pair[1];
      changed = true;
    });
    if (changed) { state.emailReceiveLiveRevision++; }
    return changed ? "changed" : false;
  }

  function setSSEState(up) {
    var dot = $("live-dot");
    dot.classList.toggle("up", up === true);
    dot.classList.toggle("down", up === false);
    var label = up === true ? "Connected" : (up === false ? "Reconnecting" : "Connecting");
    if ($("live-label").textContent !== label) { $("live-label").textContent = label; }
    $("status-sse").textContent = "sse " + (up === true ? "connected" : (up === false ? "reconnecting" : "idle"));
  }

  // Upstream degradation from the SSE "upstream" channel: the segment lists
  // the erroring sources while any is down and clears on recovery. Text goes
  // in via textContent/title assignment only, so the server-supplied error
  // text stays inert markup-wise.
  function renderUpstreamStatus() {
    var node = $("status-upstream");
    var sources = Object.keys(state.upstreamErrors).sort();
    var label = sources.length ? "Upstream degraded: " + sources.join(", ") + ". Retained data may be stale." : "";
    if (node.textContent !== label) { node.textContent = label; }
    var top = $("source-status");
    if (top && top.textContent !== label) { top.textContent = label; }
    if (!sources.length) { node.removeAttribute("title"); return; }
    node.title = sources.map(function (source) {
      return source + ": " + state.upstreamErrors[source];
    }).join("\n");
  }

  // --- server-sent events ----------------------------------------------
  function openEvents(transcriptID, afterSequence, withMessages, withMemories, withFacts, withSecrets, withEmail, emailUnread, emailUnacked, withEmailSent) {
    withMessages = withMessages === true;
    withMemories = withMemories === true;
    withFacts = withFacts === true;
    withSecrets = withSecrets === true;
    withEmail = withEmail === true;
    withEmailSent = withEmailSent === true;
    emailUnread = withEmail && emailUnread === true;
    emailUnacked = withEmail && emailUnacked === true;
    if (state.eventSource && state.sseTranscript === (transcriptID || null) &&
        state.sseMessages === withMessages && state.sseMemories === withMemories &&
        state.sseFacts === withFacts && state.sseSecrets === withSecrets &&
        state.sseEmail === withEmail && state.sseEmailSent === withEmailSent &&
        state.sseEmailUnread === emailUnread &&
        state.sseEmailUnacked === emailUnacked) { return; }
    if (state.eventSource) { state.eventSource.close(); }
    var params = [];
    if (transcriptID) {
      params.push("transcript=" + encodeURIComponent(transcriptID));
      // Seed the server's poll cursor at our highest rendered entry so the
      // stream starts at the live edge instead of replaying the transcript.
      if (afterSequence > 0) { params.push("after_sequence=" + encodeURIComponent(afterSequence)); }
    }
    if (withMessages) { params.push("messages=true"); }
    if (withMemories) { params.push("memories=true"); }
    if (withFacts) { params.push("facts=true"); }
    if (withSecrets) { params.push("secrets=true"); }
    if (withEmail) {
      params.push("email=true");
      if (emailUnread) { params.push("email_unread=true"); }
      if (emailUnacked) { params.push("email_unacked=true"); }
    }
    if (withEmailSent) { params.push("email_sent=true"); }
    var source = new EventSource("/api/events" + (params.length ? "?" + params.join("&") : ""));
    state.eventSource = source;
    state.sseTranscript = transcriptID || null;
    state.sseMessages = withMessages;
    state.sseMemories = withMemories;
    state.sseFacts = withFacts;
    state.sseSecrets = withSecrets;
    state.sseEmail = withEmail;
    state.sseEmailSent = withEmailSent;
    state.sseEmailUnread = emailUnread;
    state.sseEmailUnacked = emailUnacked;
    // Digests belong to one stream shape. Direct reads may have changed the
    // rendered state while another panel owned the EventSource, so the first
    // frame of each newly opened direction must be applied even when its wire
    // payload matches a frame seen during an earlier visit.
    if (withEmail) { state.lastEmailData = null; }
    if (withEmailSent) { state.lastEmailSentData = null; }
    // Each stream's tracker starts fresh server-side and re-announces any
    // still-failing source, so stale degradation must not carry over. The
    // same reset runs in onopen: a browser auto-reconnect reuses this
    // EventSource without re-running openEvents, and a source that recovered
    // while disconnected emits no clearing event — the fresh tracker only
    // announces failures — so it would otherwise stay red forever.
    state.upstreamErrors = {};
    renderUpstreamStatus();
    source.onopen = function () {
      if (state.eventSource !== source) { return; }
      // Browser auto-reconnect keeps this EventSource object but the server
      // creates a fresh upstream tracker. Reapply the first healthy frame even
      // when its bytes equal the last frame from the prior HTTP connection,
      // so a latched degraded state can clear without data changing.
      if (withEmail) { state.lastEmailData = null; }
      if (withEmailSent) { state.lastEmailSentData = null; }
      setSSEState(true);
      state.upstreamErrors = {};
      renderUpstreamStatus();
    };
    source.onerror = function () {
      if (state.eventSource === source) { setSSEState(false); }
    };
    source.addEventListener("upstream", function (event) {
      if (state.eventSource !== source) { return; }
      var body;
      try { body = JSON.parse(event.data); } catch (_) { return; }
      if (!body || !body.source) { return; }
      if (body.ok) { delete state.upstreamErrors[body.source]; }
      else { state.upstreamErrors[body.source] = String(body.message || "upstream error"); }
      if (body.source === "email") {
        state.emailReceiveLiveRevision++;
        state.emailReceiveDegraded = !body.ok;
        if (!body.ok && state.emailAvailable === null) {
          state.emailAvailable = false;
          state.emailReceiveUnavailableReason = "upstream";
        }
      }
      if (body.source === "email_sent") {
        state.emailSentLiveRevision++;
        state.emailSentDegraded = !body.ok;
        if (!body.ok && state.emailSentAvailable === null) {
          state.emailSentAvailable = false;
          state.emailSentUnavailableReason = "upstream";
        }
      }
      renderUpstreamStatus();
      if ((body.source === "email" || body.source === "email_sent") &&
          parseHash().section === "email") { renderEmailList(); }
      if (body.source === "email" && body.ok) {
        retryEmailAddressAfterRecovery(state.emailViewGeneration);
      }
    });
    source.addEventListener("self", function (event) {
      if (state.eventSource !== source) { return; }
      setSSEState(true);
      // The digest carries no clock, so identical frames mean nothing
      // changed; skip the no-op re-render.
      if (event.data === state.lastSelfData) { return; }
      state.lastSelfData = event.data;
      var self;
      try { self = JSON.parse(event.data); } catch (_) { return; }
      var emailStateChanged = updateEmailAddressFromCheckpoint(self.email_checkpoint);
      renderHeader(self);
      var current = parseHash();
      if (current.section === "overview") { renderOverview(self); }
      if (emailStateChanged && current.section === "email") {
        if (emailStateChanged === "disabled") {
          renderEmailUnavailable("feature_disabled");
          // Receive policy is independent from sent history. Reopen without
          // the receive poll while retaining the sent lifecycle subscription.
          openEmailEvents(state.emailViewGeneration);
        } else if (emailStateChanged === "reenabled") {
          probeEmailMailbox(state.emailViewGeneration);
        } else {
          renderEmailList();
          // A receive-state checkpoint may win the race with the initial
          // status/list GET. That older GET is correctly discarded by the
          // live revision fence, so the checkpoint itself must also upgrade
          // the existing sent-only stream to include received-email frames.
          openEmailEvents(state.emailViewGeneration);
        }
      }
    });
    source.addEventListener("memories", function (event) {
      if (event.data === state.lastMemoriesData) { return; }
      state.lastMemoriesData = event.data;
      var current = parseHash();
      if (current.section !== "memories" || current.id) { return; }
      var page;
      try { page = JSON.parse(event.data); } catch (_) { return; }
      renderMemoriesList(page);
    });
    source.addEventListener("facts", function (event) {
      if (event.data === state.lastFactsData) { return; }
      state.lastFactsData = event.data;
      var body;
      try { body = JSON.parse(event.data); } catch (_) { return; }
      mergeFacts(body.facts);
      var current = parseHash();
      if (current.section !== "facts" || current.id) { return; }
      // Re-rendering the list also discards any value the user revealed:
      // revealed values live only in the replaced DOM.
      renderFactsList(body.facts || []);
    });
    source.addEventListener("secrets", function (event) {
      if (event.data === state.lastSecretsData) { return; }
      state.lastSecretsData = event.data;
      var current = parseHash();
      if (current.section !== "secrets" || current.id) { return; }
      var body;
      try { body = JSON.parse(event.data); } catch (_) { return; }
      renderSecretsList(body.secrets || []);
    });
    source.addEventListener("transcript", function (event) {
      if (!transcriptID) { return; }
      var page;
      try { page = JSON.parse(event.data); } catch (_) { return; }
      appendEntries(transcriptID, page.entries || []);
    });
    source.addEventListener("messages", function (event) {
      if (state.eventSource !== source) { return; }
      var pages;
      try { pages = JSON.parse(event.data); } catch (_) { return; }
      var changedIn = mergeMessages(pages.inbox, "received");
      var changedOut = mergeMessages(pages.outbox, "sent");
      if (!changedIn && !changedOut) { return; }
      var current = parseHash();
      if (current.section !== "conversations") { return; }
      if (current.id) { renderConversation(decodeURIComponent(current.id)); }
      else { renderConversationList(); }
    });
    source.addEventListener("email", function (event) {
      if (state.eventSource !== source) { return; }
      // The server bundles value-free capacity into the existing email tick.
      // Skip identical frames so the poll cadence never becomes a DOM-render
      // cadence, while a new message or admin limit change still refreshes the
      // visible pane immediately.
      if (event.data === state.lastEmailData) { return; }
      state.lastEmailData = event.data;
      state.emailReceiveLiveRevision++;
      var body;
      try { body = JSON.parse(event.data); } catch (_) { return; }
      if (body.available === false) {
        settleReceivedEmailUnavailable(body.reason || "unavailable", true);
        if (parseHash().section === "email") {
          renderEmailUnavailable(state.emailReceiveUnavailableReason);
          // Settled policy/enrollment/build states return to a sent-only
          // stream. Transient availability keeps email=true so the next
          // successful live frame repairs the pane without a reload.
          openEmailEvents(state.emailViewGeneration);
        }
        return;
      }
      state.emailAvailable = true;
      state.emailReceiveUnavailableReason = null;
      state.emailReceiveDegraded = false;
      if (Object.prototype.hasOwnProperty.call(body, "status")) {
        state.emailStatus = body.status || null;
      }
      state.emailMessages = body.messages || [];
      if (parseHash().section === "email") {
        renderEmailList();
        retryEmailAddressAfterRecovery(state.emailViewGeneration);
      }
    });
    source.addEventListener("email_sent", function (event) {
      if (state.eventSource !== source) { return; }
      // Sent availability and lifecycle are deliberately independent from the
      // inbound checkpoint and receive filters. A disabled sent feature stays
      // subscribed so a live policy re-enable can recover without reinstall.
      if (event.data === state.lastEmailSentData) { return; }
      state.lastEmailSentData = event.data;
      state.emailSentLiveRevision++;
      var body;
      try { body = JSON.parse(event.data); } catch (_) { return; }
      if (body.available === false) {
        settleSentEmailUnavailable(body.reason || "unavailable");
      } else {
        state.emailSentAvailable = true;
        state.emailSentUnavailableReason = null;
        state.emailSentDegraded = false;
        state.emailSentMessages = body.messages || [];
      }
      if (parseHash().section === "email") { renderEmailList(); }
    });
  }

  // --- routing ----------------------------------------------------------
  function parseHash() {
    var hash = window.location.hash || "#/overview";
    var query = {};
    var q = hash.indexOf("?");
    if (q >= 0) {
      new URLSearchParams(hash.slice(q + 1)).forEach(function (value, key) { query[key] = value; });
      hash = hash.slice(0, q);
    }
    var parts = hash.replace(/^#\//, "").split("/").filter(Boolean);
    if (parts[0] === "account") {
      // Preserve the ticket segment, rejecting query strings, extra segments and
      // encoded separators. Account routes never carry payloads or credentials.
      var ticket = null;
      try { ticket = parts[2] ? decodeURIComponent(parts[2]) : null; } catch (_) { return { section: "account", invalid: true }; }
      return { section: "account", subsection: parts[1] || "overview", ticket: ticket,
        invalid: q >= 0 || parts.length > 3 || (ticket !== null && (parts[1] !== "support" || !accountID(ticket))) };
    }
    return { section: parts[0] || "overview", id: parts[1] || null, query: query };
  }

  function breadcrumb(items) {
    $("breadcrumb").innerHTML = items.map(function (item, index) {
      if (index === items.length - 1) { return '<span class="here">' + esc(item.label) + "</span>"; }
      return '<a href="' + esc(item.href) + '">' + esc(item.label) + "</a> / ";
    }).join("");
  }

  function setNav(section) {
    document.querySelectorAll(".rail a").forEach(function (link) {
      link.classList.toggle("active", link.getAttribute("data-nav") === section);
    });
  }

  // Every visit owns its Overview, fact, memory and transcript reads, including the same URL.
  var overviewViewGeneration = 0;
  var factViewGeneration = 0;
  var memoryViewGeneration = 0;
  var transcriptViewGeneration = 0;
  var secretViewGeneration = 0;

  function route() {
    cancelAccountView();
    stopSummary();
    overviewViewGeneration++;
    factViewGeneration++;
    memoryViewGeneration++;
    transcriptViewGeneration++;
    secretViewGeneration++;
    var viewGeneration = invalidateEmailView();
    invalidateMessageBodyView();
    var current = parseHash();
    focusRouteChanged(current);
    setNav(current.section);
    if (current.section === "account") { return viewAccount(current); }
    if (current.section === "transcripts" && current.id) { return viewTranscript(current.id, current.query); }
    if (current.section === "transcripts") { return viewTranscripts(); }
    if (current.section === "facts" && current.id) { return viewFact(current.id); }
    if (current.section === "facts") { return viewFacts(); }
    if (current.section === "memories" && current.id) { return viewMemory(current.id); }
    if (current.section === "memories") { return viewMemories(); }
    if (current.section === "secrets" && current.id) { return viewSecret(current.id); }
    if (current.section === "secrets") { return viewSecrets(); }
    if (current.section === "conversations" && current.id) { return viewConversation(decodeURIComponent(current.id)); }
    if (current.section === "conversations") { return viewConversations(); }
    if (current.section === "email") { return viewEmail(viewGeneration); }
    return viewOverview();
  }

  function showError(err) {
    $("view").innerHTML = '<div class="error">' + esc(err.message || err) + "</div>";
  }

  var toastTimer = null;

  function toast(message) {
    var node = document.getElementById("toast");
    if (!node) {
      node = document.createElement("div");
      node.id = "toast";
      node.className = "toast";
      document.body.appendChild(node);
    }
    node.textContent = message;
    node.classList.add("show");
    if (toastTimer) { clearTimeout(toastTimer); }
    toastTimer = setTimeout(function () {
      toastTimer = null;
      node.classList.remove("show");
    }, 3500);
  }

  // --- Account: separate authority and short-lived selected reads --------
  var ACCOUNT_SCHEMA = "witself.console.account.v1";
  var accountSections = ["overview", "clients", "plan", "billing", "support", "access"];
  var accountLabels = ["Overview", "Clients on this device", "Plan & limits", "Billing", "Support", "Access"];
  // Only this allowlisted identity survives between views. Section payloads,
  // especially ticket bodies, are never cached, filtered, logged or persisted.
  var accountContext = null;
  var accountContextKnown = false;
  var accountContextSequence = 0;
  var accountContextController = null;
  var accountTimer = null;
  var accountStarted = false;
  var accountGeneration = 0;
  var accountViewController = null;
  var accountViewActive = false;
  var accountBusy = false;

  function accountID(value) {
    return typeof value === "string" && value.length > 0 && value.length <= 256 &&
      value !== "." && value !== ".." && !/[\s/\\%?#:\x00-\x1f\x7f]/.test(value);
  }
  function verifiedAccountContext(data) {
    if (!data || data.schema_version !== ACCOUNT_SCHEMA || data.available !== true ||
        !accountID(data.account_id) || !accountID(data.operator_id) ||
        ["account_owner", "account_admin", "account_billing", "account_operator"].indexOf(data.role) < 0 ||
        !Array.isArray(data.sections) || !data.sections.length) { return null; }
    var previous = -1;
    for (var i = 0; i < data.sections.length; i++) {
      var index = accountSections.indexOf(data.sections[i]);
      if (index <= previous || (data.role === "account_operator" && (index === 2 || index === 3))) { return null; }
      previous = index;
    }
    return { account_id: data.account_id, operator_id: data.operator_id, role: data.role, sections: data.sections.slice() };
  }
  function cancelAccountView() {
    accountGeneration++;
    if (accountViewController) { accountViewController.abort(); }
    accountViewController = null;
    accountBusy = false;
    // Clear the actual nodes before dropping the view owner. No retained HTML
    // or private thread object exists to restore them on a later visit.
    if (accountViewActive) { $("view").textContent = ""; $("breadcrumb").textContent = ""; }
    accountViewActive = false;
  }
  function accountNavigation() {
    var node = $("account-nav");
    if (!node) { return; }
    if (!accountContext) { node.textContent = ""; }
    else if (!node.querySelector("a")) { node.innerHTML = '<div class="account-rail"><a href="#/account" data-nav="account">Account</a></div>'; }
    setNav(parseHash().section);
  }
  function accountFallback() {
    cancelAccountView();
    window.location.hash = "#/overview";
    setNav("overview");
    breadcrumb([{ label: "overview" }]);
    // The ordinary hashchange handler mounts the agent Overview.
  }
  function accountRead(path, controller) {
    var timeout;
    var deadline = new Promise(function (_, reject) {
      timeout = setTimeout(function () { controller.abort(); reject({ code: "unavailable" }); }, 10000);
    });
    var response = fetch(path, { credentials: "same-origin", cache: "no-store", signal: controller.signal })
      .then(function (resp) {
        if (resp.status === 401 || resp.status === 403) { throw { code: "forbidden" }; }
        return resp.json().then(function (data) {
          if (!resp.ok) { throw { code: data && data.error === "response_too_large" ? "response_too_large" :
            (resp.status === 401 || resp.status === 403 ? "forbidden" : "unavailable") }; }
          if (!data || data.schema_version !== ACCOUNT_SCHEMA) { throw { code: "unavailable" }; }
          return data;
        });
      });
    return Promise.race([response, deadline]).finally(function () { clearTimeout(timeout); });
  }
  function dropAccountTicket() {
    var current = parseHash();
    if (current.section === "account" && current.ticket) {
      // Replace rather than push: returning to this visit must not reopen a body.
      window.history.replaceState(null, "", "#/account/support");
      current.ticket = null;
    }
    return current;
  }
  function checkAccountContext() {
    if (document.hidden) { return Promise.resolve(); }
    clearTimeout(accountTimer);
    var sequence = ++accountContextSequence;
    if (accountContextController) { accountContextController.abort(); }
    var controller = accountContextController = new AbortController();
    function accept(next) {
      if (sequence !== accountContextSequence || document.hidden) { return; }
      var changed = JSON.stringify(accountContext) !== JSON.stringify(next);
      var first = !accountContextKnown;
      if (changed || !next) {
        if (accountContextKnown) { dropAccountTicket(); }
        cancelAccountView();
      }
      accountContext = next;
      accountContextKnown = true;
      accountNavigation();
      if (parseHash().section === "account") {
        if (!next) { accountFallback(); }
        else if (changed || first) { route(); }
      }
    }
    return accountRead("/api/account/context", controller).then(function (data) {
      accept(verifiedAccountContext(data));
    }).catch(function () { accept(null); }).finally(function () {
      if (sequence !== accountContextSequence) { return; }
      accountContextController = null;
      if (accountStarted && !document.hidden) { accountTimer = setTimeout(checkAccountContext, 5000); }
    });
  }
  function stopAccount() {
    dropAccountTicket();
    accountStarted = false;
    clearTimeout(accountTimer);
    accountContextSequence++;
    if (accountContextController) { accountContextController.abort(); }
    accountContextController = null;
    cancelAccountView();
    accountContext = null;
    accountContextKnown = false;
    accountNavigation();
  }
  function accountVisibilityChanged() {
    if (document.hidden) {
      var started = accountStarted;
      stopAccount();
      accountStarted = started;
    } else if (accountStarted) { checkAccountContext(); }
  }
  function startAccount() {
    if (accountStarted) { return; }
    // Without cancellation and bounded deadlines this surface cannot safely
    // maintain authority. Older embedded clients retain their agent views.
    if (typeof AbortController !== "function" || typeof setTimeout !== "function" || typeof clearTimeout !== "function") {
      accountContextKnown = true;
      return;
    }
    accountStarted = true;
    document.addEventListener("visibilitychange", accountVisibilityChanged);
    window.addEventListener("pagehide", function () {
      var started = accountStarted;
      stopAccount();
      accountStarted = started;
    });
    window.addEventListener("pageshow", function (event) {
      if (event.persisted && accountStarted) { checkAccountContext(); }
    });
    checkAccountContext();
  }
  function accountText(value) {
    if (typeof value === "boolean") { return value ? "Yes" : "No"; }
    if (typeof value === "number") { return Number.isSafeInteger(value) ? String(value) : "Unknown"; }
    return typeof value === "string" && value !== "" ? value : "Unknown";
  }
  function accountRow(label, value) { return "<dt>" + esc(label) + "</dt><dd>" + esc(accountText(value)) + "</dd>"; }
  function accountRows(data, fields) {
    return '<dl class="account-kv">' + fields.map(function (field) { return accountRow(field[1], data && data[field[0]]); }).join("") + "</dl>";
  }
  function accountCard(title, body) { return '<section class="panel account-card"><h2>' + esc(title) + "</h2>" + body + "</section>"; }
  function accountNotice(text) { return '<p class="dim">' + esc(text) + "</p>"; }
  function accountTruncated(data) { return data && data.truncated === true ? accountNotice("Partial display: this source was truncated by the console read limit.") : ""; }
  function accountPending(data) {
    return data ? accountRows(data, [["kind", "Change"], ["plan", "Pending plan"], ["plan_name", "Pending plan name"],
      ["requested", "Requested"], ["effective", "Effective"], ["expires", "Expires"]]) : accountNotice("No pending change reported.");
  }
  function accountMoneyText(data) {
    var currency = data && typeof data.currency === "string" && data.currency ? data.currency : "Unknown currency";
    var cents = data && data.amount_cents;
    if (!Number.isSafeInteger(cents)) { return "Unknown amount · " + currency; }
    // Arithmetic in integer space avoids rounding even at MAX_SAFE_INTEGER.
    var exact = BigInt(cents), magnitude = exact < 0n ? -exact : exact;
    return currency + " " + (exact < 0n ? "-" : "") + String(magnitude / 100n) + "." +
      String(magnitude % 100n).padStart(2, "0") + " (" + String(exact) + " cents)";
  }
  function accountRetention(data, key) {
    if (!data || !Object.prototype.hasOwnProperty.call(data, key)) { return "Unknown"; }
    if (data[key] === null) { return "Indefinite"; }
    return Number.isSafeInteger(data[key]) && data[key] > 0 ? data[key] + " days" : "Unknown";
  }
  function accountPlanHTML(data) {
    var html = accountCard("Current plan", accountRows(data, [["plan", "Current plan"], ["plan_name", "Current plan name"],
      ["billing_plan", "Billing plan"], ["billing_plan_name", "Billing plan name"], ["applied", "Applied plan"],
      ["apply_pending", "Application pending"], ["apply_blocked", "Application blocked"], ["past_due_since", "Past due since"]]));
    html += accountCard("Pending change", accountPending(data.pending));
    // The core projection supplies the closed, full unit maps for each valid
    // source. Never duplicate its catalog or infer a value from malformed data.
    function limitMap(value) { return value && typeof value === "object" && !Array.isArray(value) ? value : null; }
    function numericKeys(source) { return Object.keys(source || {}).filter(function (key) {
      return Number.isSafeInteger(source[key]) && source[key] >= 0;
    }); }
    var limits = limitMap(data.limits), defaults = limitMap(data.limit_defaults);
    var units = limitMap(data.limits_units), defaultUnits = limitMap(data.limit_defaults_units);
    var keys = Array.from(new Set(Object.keys(units || {}).concat(Object.keys(defaultUnits || {}),
      numericKeys(limits), numericKeys(defaults)))).sort();
    html += accountCard("Limits", keys.map(function (key) {
      function limit(source, units) {
        if (!source) { return "Unknown"; }
        if (!Object.prototype.hasOwnProperty.call(source, key)) { return "No plan cap"; }
        return Number.isSafeInteger(source[key]) && source[key] >= 0 ? source[key] + " " + accountText(units && units[key]) : "Unknown";
      }
      return '<div class="account-item"><h3>' + esc(key.replace(/_/g, " ")) + '</h3><dl class="account-kv">' +
        accountRow("Effective limit", limit(limits, data.limits_units)) + accountRow("Plan default", limit(defaults, data.limit_defaults_units)) + "</dl></div>";
    }).join("") || accountNotice("Limits unknown."));
    var features = Array.isArray(data.features) ? data.features : null;
    var featureDefaults = Array.isArray(data.feature_defaults) ? data.feature_defaults : null;
    html += accountCard("Features", accountRows({ effective: features ? features.join(", ") || "None" : null,
      defaults: featureDefaults ? featureDefaults.join(", ") || "None" : null }, [["effective", "Effective features"], ["defaults", "Plan defaults"]]) +
      ["messaging", "email_receive", "email_send"].map(function (key) {
        return '<div class="account-item"><h3>' + esc(key.replace(/_/g, " ")) + "</h3>" + accountRows(data[key],
          [["enabled", "Enabled"], ["default_enabled", "Plan default"], ["overridden", "Override applied"]]) + "</div>";
      }).join(""));
    html += accountCard("Retention", ["message_retention", "email_retention", "transcript_retention"].map(function (key) {
      return '<div class="account-item"><h3>' + esc(key.replace(/_/g, " ")) + '</h3><dl class="account-kv">' +
        accountRow("Effective retention", accountRetention(data[key], "effective_days")) +
        accountRow("Plan default", accountRetention(data[key], "default_days")) + accountRow("Override applied", data[key] && data[key].overridden) + "</dl></div>";
    }).join(""));
    return html + accountTruncated(data);
  }
  function accountBillingHTML(data) {
    return ["summary", "invoices", "payments"].map(function (key) {
      var source = data[key], title = { summary: "Billing summary", invoices: "Invoices", payments: "Payments" }[key];
      if (!source || source.available !== true) {
        return accountCard(title, accountNotice(source && source.error === "response_too_large" ?
          "This source exceeds the console read limit." : "Source unavailable. This is not an empty history."));
      }
      var html;
      if (key === "summary") {
        html = accountRows(source, [["billing_available", "Billing available"], ["configured", "Configured"],
          ["subscription_status", "Subscription status"], ["billing_plan", "Billing plan"], ["billing_plan_name", "Billing plan name"],
          ["effective_plan", "Effective plan"], ["effective_plan_name", "Effective plan name"], ["applied_plan", "Applied plan"],
          ["entitled_at", "Entitled at"], ["past_due_since", "Past due since"]]) +
          '<dl class="account-kv">' + accountRow("Payment method", source.payment_method && source.payment_method.label) +
          accountRow("Next charge", accountMoneyText(source.next_charge)) + accountRow("Charge date", source.next_charge && source.next_charge.date) + "</dl>" +
          "<h3>Pending change</h3>" + accountPending(source.pending);
      } else {
        html = !Array.isArray(source.entries) ? accountNotice("History unknown.") : source.entries.length ? source.entries.map(function (row) {
          return '<div class="account-item">' + accountRows(row, key === "invoices" ?
            [["number", "Invoice"], ["date", "Date"], ["status", "Status"]] : [["date", "Date"], ["method", "Method"], ["status", "Status"]]) +
            '<dl class="account-kv">' + accountRow("Amount", accountMoneyText(row)) + "</dl></div>";
        }).join("") : accountNotice("No " + key + " returned.");
      }
      return accountCard(title, html + accountTruncated(source));
    }).join("");
  }
  function accountClientsHTML(data) {
    var report = data.report;
    var html = accountNotice("Recorded clients for this account on this device. Check configuration without launching a client.") +
      accountNotice("Recorded versions and check times do not indicate activity. Connection testing is not run.") +
      '<button id="account-scan" type="button">Check this device</button>';
    if (!report || data.status === "not_checked") { return accountCard("Clients on this device", html + accountNotice("Not checked. Use Check this device to inspect local metadata.")); }
    html += accountRows(report, [["checked_at", "Local checked time"], ["scan_status", "Check status"]]);
    if (report.scan_status === "partial" || report.scan_status === "unavailable") { html += accountNotice("Local metadata is incomplete or unavailable; this is not a complete inventory."); }
    html += !Array.isArray(report.entries) ? accountNotice("Client metadata unknown.") : report.entries.length ? report.entries.map(function (row) {
      return '<div class="account-item"><h3>' + esc(accountText(row.runtime)) + "</h3>" + accountRows(row,
        [["recorded_version", "Recorded installation version"], ["executable_status", "Recorded executable"], ["configuration_status", "Configuration status"],
          ["installed_at", "Recorded installation time"]]) + '<dl class="account-kv">' +
        accountRow("Configuration scope", row.configuration_scope === "mcp_registration" ? "Recorded MCP registration only" : "None") +
        accountRow("Effective verification", "Not run") + "</dl></div>";
    }).join("") : accountNotice("No matching recorded clients returned for this account on this device.");
    return accountCard("Clients on this device", html + accountTruncated(report));
  }
  var accountTicketFields = [["id", "Ticket"], ["subject", "Subject"], ["category", "Category"], ["state", "State"],
    ["priority", "Priority"], ["opened_at", "Opened"], ["first_response_at", "First response"], ["resolved_at", "Resolved"],
    ["closed_at", "Closed"], ["last_activity_at", "Last activity"]];
  function accountSupportHTML(data, ticket) {
    if (ticket) {
      if (!data.ticket || data.ticket.id !== ticket) { return accountNotice("Selected thread unavailable."); }
      return accountCard("Selected support ticket", '<a href="#/account/support">Back to ticket metadata</a>' + accountRows(data.ticket, accountTicketFields)) +
        accountCard("Thread", accountNotice("Selected read only. Thread text is cleared when you leave, refresh or hide this page.") +
          (!Array.isArray(data.messages) ? accountNotice("Thread unknown.") : data.messages.length ? data.messages.map(function (message) {
            return '<article class="account-item">' + accountRows(message, [["author_kind", "Author"], ["posted_at", "Posted"]]) +
              '<p class="account-thread">' + esc(accountText(message.body)) + "</p></article>";
          }).join("") : accountNotice("No messages returned.")) + accountTruncated(data));
    }
    return accountCard("Support", accountNotice("Ticket metadata only. Select a ticket to read its bounded thread.") +
      (!Array.isArray(data.tickets) ? accountNotice("Ticket metadata unknown.") : data.tickets.length ? data.tickets.map(function (row) {
        return '<div class="account-item">' + (accountID(row.id) ? '<a href="#/account/support/' + esc(encodeURIComponent(row.id)) + '">Read ticket</a>' : "") +
          accountRows(row, accountTicketFields) + "</div>";
      }).join("") : accountNotice("No support tickets returned.")) + accountTruncated(data));
  }
  function accountSectionHTML(data, current) {
    if (current.subsection === "plan") { return accountPlanHTML(data); }
    if (current.subsection === "billing") { return accountBillingHTML(data); }
    if (current.subsection === "clients") { return accountClientsHTML(data); }
    if (current.subsection === "support") { return accountSupportHTML(data, current.ticket); }
    if (current.subsection === "access") {
      return accountCard("Access", accountRows(data, [["account_id", "Account scope"], ["operator_id", "Current CLI manager"], ["role", "Manager role (raw)"]]) +
        accountNotice("Available read sections: " + data.sections.map(function (key) { return accountLabels[accountSections.indexOf(key)]; }).join(", ")) +
        accountNotice("These reads use the verified CLI manager for this account. They do not grant the selected agent account permissions."));
    }
    return accountCard("Account profile", accountRows(data.account, [["id", "Account ID"], ["display_name", "Display name"], ["status", "Status"],
      ["email", "Contact email"], ["created_at", "Created"]]) + accountTruncated(data));
  }
  function accountShell(current) {
    breadcrumb([{ label: "Account", href: "#/account" }, { label: accountLabels[accountSections.indexOf(current.subsection)] }]);
    var role = { account_owner: "Owner", account_admin: "Administrator", account_billing: "Billing", account_operator: "Operator" }[accountContext.role];
    $("view").innerHTML = '<div class="account-area"><section class="panel account-header">' +
      '<div class="account-heading"><h1>Account</h1><span class="badge">Read-only</span></div>' +
      '<div class="account-scope">Account ID: <span class="mono">' + esc(accountContext.account_id) +
      '</span> · Manager role: ' + esc(role) + '</div><nav class="account-sections" aria-label="Account sections">' +
      accountContext.sections.map(function (key) { return '<a href="#/account/' + key + '"' +
        (key === current.subsection ? ' aria-current="page"' : "") + '>' + accountLabels[accountSections.indexOf(key)] + "</a>"; }).join("") +
      '</nav><button id="account-refresh" type="button">Refresh selected section</button></section>' +
      '<div id="account-content" role="status" aria-live="polite">Loading…</div></div>';
    $("account-refresh").addEventListener("click", function () {
      if (!accountBusy) { loadAccountSection(dropAccountTicket(), false); }
    });
  }
  function viewAccount(current) {
    accountViewActive = true;
    // Stop section-specific agent subscriptions while retaining header updates.
    openEvents(null);
    if (document.hidden) { return; }
    if (!accountContext) {
      if (accountContextKnown) { accountFallback(); }
      else { $("view").textContent = "Verifying account access…"; $("breadcrumb").textContent = ""; }
      return;
    }
    if (current.invalid || accountContext.sections.indexOf(current.subsection) < 0) { accountFallback(); return; }
    accountShell(current);
    return loadAccountSection(current, false);
  }
  function loadAccountSection(current, scan) {
    if (accountBusy || !accountContext || document.hidden || accountContext.sections.indexOf(current.subsection) < 0) { return Promise.resolve(); }
    var generation = accountGeneration;
    var accessSequence = null;
    if (current.subsection === "access") {
      accessSequence = ++accountContextSequence;
      clearTimeout(accountTimer);
      if (accountContextController) { accountContextController.abort(); }
      accountContextController = null;
    }
    var actionID = scan ? "account-scan" : "account-refresh";
    var action = $(actionID), restoreFocus = document.activeElement === action;
    function focusMoved(event) { if (event.target !== action) { restoreFocus = false; } }
    document.addEventListener("focusin", focusMoved);
    var controller = accountViewController = new AbortController();
    accountBusy = true;
    $("account-refresh").disabled = true;
    var button = $("account-scan");
    if (button) { button.disabled = true; }
    // Explicit refresh/scan immediately removes all previously displayed data.
    $("account-content").textContent = scan ? "Checking this device…" : "Loading…";
    var path = "/api/account/" + current.subsection + (scan ? "/scan" : current.ticket ? "/" + encodeURIComponent(current.ticket) : "");
    function live() { return generation === accountGeneration && accountViewActive && !document.hidden && !controller.signal.aborted; }
    return accountRead(path, controller).then(function (data) {
      if (!live()) { return; }
      if (accessSequence !== null) {
        if (accessSequence !== accountContextSequence) { throw { code: "superseded" }; }
        var next = verifiedAccountContext(data);
        if (!next) { throw { code: "forbidden" }; }
        var changed = JSON.stringify(accountContext) !== JSON.stringify(next);
        accountContext = next;
        accountContextKnown = true;
        accountNavigation();
        if (next.sections.indexOf(current.subsection) < 0) { accountFallback(); return; }
        if (changed) { accountShell(current); $("account-refresh").disabled = true; }
      }
      if (data.available !== true) { throw { code: "unavailable" }; }
      $("account-content").innerHTML = accountSectionHTML(data, current);
      var scanButton = $("account-scan");
      if (scanButton) { scanButton.addEventListener("click", function () { loadAccountSection(current, true); }); }
    }).catch(function (error) {
      // A timeout aborts this controller too; generation is the authority fence.
      if (generation !== accountGeneration || !accountViewActive || document.hidden) { return; }
      if (accessSequence !== null && accessSequence !== accountContextSequence) { error = { code: "superseded" }; }
      if ((error && error.code === "forbidden") || (accessSequence !== null && accessSequence === accountContextSequence)) {
        cancelAccountView(); accountContext = null; accountContextKnown = true; accountNavigation(); accountFallback();
        checkAccountContext();
        return;
      }
      $("account-content").textContent = error && error.code === "response_too_large" ?
        (current.ticket ? "This thread exceeds the console read limit (4 MiB). Inspect it deliberately with the existing support CLI." : "This source exceeds the console read limit.") :
        "This section is unavailable. No successful empty result was returned.";
      if (scan) {
        var content = $("account-content");
        content.innerHTML = accountNotice(content.textContent) + '<button id="account-scan" type="button">Check this device</button>';
        $("account-scan").addEventListener("click", function () { loadAccountSection(current, true); });
      }
    }).finally(function () {
      document.removeEventListener("focusin", focusMoved);
      if (accessSequence !== null && accessSequence === accountContextSequence && accountStarted && !document.hidden) {
        accountTimer = setTimeout(checkAccountContext, 5000);
      }
      if (generation !== accountGeneration) { return; }
      accountBusy = false;
      accountViewController = null;
      var refresh = $("account-refresh");
      if (refresh) { refresh.disabled = false; }
      var target = $(actionID);
      if (restoreFocus && accountViewActive && !document.hidden && target &&
          (document.activeElement === action || document.activeElement === document.body)) {
        target.focus({ preventScroll: true });
      }
    });
  }

  // --- views ------------------------------------------------------------
  function memoryCapacityHTML(capacity) {
    if (!capacity) { return ""; }
    if (capacity.unavailable) {
      return '<div class="panel"><h2>Active memory capacity</h2><div class="dim">capacity status temporarily unavailable</div></div>';
    }
    var used = Math.max(0, Number(capacity.used) || 0);
    var level = capacity.over_limit || capacity.at_limit ? "danger" : (capacity.near_limit ? "warning" : "");
    var stateLabel = capacity.over_limit ? "over limit" : (capacity.at_limit ? "at limit" : (capacity.near_limit ? "near limit" : "available"));
    if (capacity.unlimited) {
      return '<div class="panel"><h2>Active memory capacity</h2><div class="capacity-line"><a href="#/memories">' +
        esc(used) + ' active</a><span class="badge">unlimited</span></div></div>';
    }
    var maximum = Math.max(0, Number(capacity.max) || 0);
    var remaining = Math.max(0, Number(capacity.remaining) || 0);
    return '<div class="panel capacity ' + level + '"><h2>Active memory capacity</h2>' +
      '<div class="capacity-line"><a href="#/memories">' + esc(used) + " of " + esc(maximum) +
      ' active</a><span class="badge">' + esc(stateLabel) + "</span></div>" +
      '<progress class="capacity-track" aria-label="active memory capacity" max="' +
      esc(maximum || 1) + '" value="' + esc(Math.min(used, maximum || 1)) + '"></progress>' +
      '<div class="dim">' + esc(remaining) + " remaining · safe consolidation and replacement stay available at the limit</div></div>";
  }

  function factCapacityHTML(capacity) {
    if (!capacity) { return ""; }
    if (capacity.unavailable) {
      return '<div class="panel"><h2>Current fact capacity</h2><div class="dim">capacity status temporarily unavailable · fact reads and existing-fact updates remain available</div></div>';
    }
    var used = Math.max(0, Number(capacity.used) || 0);
    var level = capacity.over_limit || capacity.at_limit ? "danger" : (capacity.near_limit ? "warning" : "");
    var stateLabel = capacity.over_limit ? "over limit" : (capacity.at_limit ? "at limit" : (capacity.near_limit ? "near limit" : "available"));
    if (capacity.unlimited) {
      return '<div class="panel"><h2>Current fact capacity</h2><div class="capacity-line"><a href="#/facts">' +
        esc(used) + ' current</a><span class="badge">unlimited</span></div></div>';
    }
    var maximum = Math.max(0, Number(capacity.max) || 0);
    var remaining = Math.max(0, Number(capacity.remaining) || 0);
    return '<div class="panel capacity ' + level + '"><h2>Current fact capacity</h2>' +
      '<div class="capacity-line"><a href="#/facts">' + esc(used) + " of " + esc(maximum) +
      ' current</a><span class="badge">' + esc(stateLabel) + "</span></div>" +
      '<progress class="capacity-track" aria-label="current fact capacity" max="' +
      esc(maximum || 1) + '" value="' + esc(Math.min(used, maximum || 1)) + '"></progress>' +
      '<div class="dim">' + esc(remaining) + " remaining · existing-fact updates and separately authorized deletion remain available at the limit</div></div>";
  }

  function planEntitlementsHTML(entitlements) {
    var title = '<div class="panel"><h2>Enforced plan &amp; entitlements</h2>';
    if (!entitlements) {
      return title + '<div class="dim">applied entitlement projection is not available on this cell version</div></div>';
    }
    if (entitlements.state === "unmanaged") {
      return title + '<div class="capacity-line"><span class="badge">unmanaged</span></div>' +
        '<div class="dim">this cell has no applied plan snapshot; no plan is implied</div></div>';
    }
    if (entitlements.state !== "applied") {
      return title + '<div class="capacity-line"><span class="badge">unavailable</span></div>' +
        '<div class="dim">cell-applied entitlement status is temporarily unavailable</div></div>';
    }
    var features = entitlements.features || {};
    var featureKeys = [
      "memory", "facts", "secrets", "messaging", "collaboration",
      "agent_email_receive", "agent_email_send",
    ];
    var featureRows = featureKeys.map(function (key) {
      return '<div class="row"><span class="grow mono">' + esc(key) + '</span><span class="badge">' +
        (features[key] === true ? "enabled" : "disabled") + "</span></div>";
    }).join("");
    var retention = entitlements.retention_days || {};
    var retentionKeys = [
      "transcript_retention_days", "message_retention_days", "agent_email_retention_days",
    ];
    var retentionRows = retentionKeys.map(function (key) {
      var value = retention[key];
      var label = value == null ? "indefinite" : (Number.isInteger(value) && value > 0 ? value + " days" : "unavailable");
      return '<div class="row"><span class="grow mono">' + esc(key) + '</span><span class="dim">' + esc(label) + "</span></div>";
    }).join("");
    return title + '<div class="capacity-line"><span class="mono">' + esc(entitlements.enforced_plan_id || "") +
      '</span><span class="badge">applied</span></div>' +
      '<h3>features</h3><div class="list">' + featureRows + "</div>" +
      '<h3>retention</h3><div class="list">' + retentionRows + "</div>" +
      '<div class="dim">source: cell-applied snapshot · display only</div></div>';
  }

  // A closed, content-free projection: never render server labels or metadata.
  var summaryCategories = [
    { key: "transactions", code: "OPS", name: "Operations", unit: "recorded operations", dimension: "operation", actions: [] },
    { key: "transcripts", code: "TRN", name: "Transcripts", unit: "entries recorded", dimension: "transcript_entry_write", actions: ["transcript updated", "transcript created"], route: "transcripts" },
    { key: "facts", code: "FCT", name: "Facts", unit: "recorded deliveries", dimension: "fact_returned", actions: [], route: "facts" },
    { key: "memories", code: "MEM", name: "Memories", unit: "memory changes", dimension: "memory_change", actions: ["memory updated", "memory created"], route: "memories" },
    { key: "secrets", code: "SEC", name: "Secrets", unit: "recorded accesses", dimension: "secret_read", actions: ["secret updated", "secret created"], route: "secrets" },
    { key: "email", code: "EML", name: "Email", unit: "accepted sends", dimension: "email_sent", actions: ["email received"], route: "email" },
    { key: "messages", code: "MSG", name: "Messages", unit: "messages sent", dimension: "message_sent", actions: ["message received"], route: "conversations" },
  ];
  var summaryState = { report: null, view: "overview", failed: false, active: false,
    timer: null, deadline: null, controller: null, request: null, generation: 0, nextAt: 0 };
  var summaryInterval = 30000;

  function summaryNumber(n) { return Number.isSafeInteger(n) && n >= 0; }
  function summaryDate(value) {
    if (typeof value !== "string" || !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/.test(value)) { return null; }
    var date = new Date(value);
    // Reject calendar rollovers such as February 30 as well as invalid dates.
    return Number.isFinite(date.getTime()) && date.toISOString().slice(0, 19) === value.slice(0, 19) ? date.toISOString() : null;
  }
  function normalizeSummary(body) {
    var s = body && body.summary;
    if (!s || s.schema !== "witself.agent-summary.v2" || s.refresh_after_seconds !== 30 ||
        !summaryDate(s.generated_at) || !Array.isArray(s.categories) || s.categories.length !== 7 ||
        !Array.isArray(s.recent) || s.recent.length > 12 || !Array.isArray(s.checkpoints) || s.checkpoints.length !== 4) {
      throw new Error("Invalid summary");
    }
    var w = s.window;
    var hour = 3600000, endHour = Math.floor(Date.parse(s.generated_at) / hour) * hour;
    if (!w || !summaryDate(w.since) || !summaryDate(w.until) || w.timezone !== "UTC" || w.bucket !== "hour" ||
        w.partial_current_bucket !== true || Date.parse(w.since) !== endHour - 23 * hour ||
        Date.parse(w.until) !== Math.floor(Date.parse(s.generated_at) / 1000) * 1000) { throw new Error("Invalid window"); }
    var categories = summaryCategories.map(function (def) {
      var matches = s.categories.filter(function (c) { return c && c.key === def.key; });
      if (matches.length !== 1) { throw new Error("Invalid categories"); }
      var raw = matches[0], inv = raw.inventory || {}, act = raw.activity || {};
      var expectedExact = def.key === "facts" || def.key === "memories";
      var inventory = { status: "unavailable", count: null, exact: false,
        label: def.key === "memories" ? "active memories" : def.key === "facts" ? "facts" : "recent records · bounded" };
      if (def.key !== "transactions" && inv.status === "disabled") { inventory.status = "disabled"; }
      if (def.key !== "transactions" && inv.status === "available" &&
          summaryNumber(inv.count) && inv.exact === expectedExact) {
        inventory.status = "available"; inventory.count = inv.count; inventory.exact = expectedExact;
      }
      if (def.key === "transactions") { inventory = { status: "not_applicable", label: "activity only" }; }
      var activity = normalizeSummaryActivity(act, def, w);
      return { def: def, inventory: inventory, activity: activity };
    });
    var recent = s.recent.filter(function (r) {
      return r && summaryCategories.some(function (c) {
        return c.key === r.key && c.actions.indexOf(r.action) >= 0;
      }) && summaryDate(r.at) && Date.parse(r.at) <= Date.parse(s.generated_at);
    }).map(function (r) { return { key: r.key, at: summaryDate(r.at), action: r.action }; })
      .sort(function (a, b) { return Date.parse(b.at) - Date.parse(a.at); });
    var checkpoints = [
      ["memory", "Memory curation"], ["message", "Messaging work"], ["email", "Email"], ["avatar", "Avatar lifecycle"],
    ].map(function (pair) {
      var found = s.checkpoints.filter(function (c) { return c && c.key === pair[0]; });
      var status = found.length === 1 && ["pending", "clear", "disabled", "unavailable"].indexOf(found[0].status) >= 0 ? found[0].status : "unavailable";
      return { label: pair[1], status: status };
    });
    return { generated: summaryDate(s.generated_at), since: summaryDate(w.since), until: summaryDate(w.until),
      categories: categories, recent: recent, checkpoints: checkpoints };
  }
  var summaryMeasures = [
    ["operation_read", "operation", "Reads"], ["operation_write", "operation", "Writes"],
    ["operation_read_record", "record", "Records read"], ["operation_write_record", "record", "Records written"],
    ["memory_created", "change", "Created"], ["memory_revised", "change", "Revised"],
    ["memory_archived", "change", "Archived"], ["memory_restored", "change", "Restored"], ["memory_deleted", "change", "Deleted"]
  ];
  function normalizeSummaryActivity(a, def, w) {
    var bad = { status: "unavailable" }, modern = def.key === "transactions" || def.key === "memories";
    if (a.unit !== def.unit || a.dimension !== def.dimension) { return bad; }
    var breakdown = a.breakdown == null ? [] : a.breakdown, coverage = a.coverage;
    if (coverage != null && (typeof coverage !== "object" || Array.isArray(coverage))) { return bad; }
    if ((a.bins != null && !Array.isArray(a.bins)) || !Array.isArray(breakdown)) { return bad; }
    if (["unavailable", "disabled", "server_update_needed"].indexOf(a.status) >= 0) {
      if ((!modern && a.status === "server_update_needed") || a.total != null || (a.bins && a.bins.length) || coverage || breakdown.length) { return bad; }
      return { status: a.status };
    }
    if (!Array.isArray(a.bins) || a.bins.length !== 24) { return bad; }
    if (a.status === "not_tracked") {
      return modern && a.total == null && coverage && coverage.tracking_since === null && coverage.partial_first_bucket === false &&
        !breakdown.length && a.bins.every(function (n) { return n === null; }) ? { status: "not_tracked" } : bad;
    }
    if (a.status !== "available" || !summaryNumber(a.total)) { return bad; }
    var tracking = null, hour = 3600000, since = Date.parse(w.since);
    if (modern) {
      if (!coverage || !summaryDate(coverage.tracking_since)) { return bad; }
      tracking = Date.parse(coverage.tracking_since);
      if (tracking <= Date.parse("0001-01-01T00:00:00Z") || tracking > Date.parse(w.until) || coverage.partial_first_bucket !== (tracking >= since)) { return bad; }
    } else if (coverage || breakdown.length) { return bad; }
    var total = 0;
    for (var i = 0; i < 24; i++) {
      var n = a.bins[i], unknown = tracking !== null && since + i * hour < Math.floor(tracking / hour) * hour;
      if (unknown) { if (n !== null) { return bad; } continue; }
      if (!summaryNumber(n) || !summaryNumber(total + n)) { return bad; } total += n;
    }
    if (total !== a.total) { return bad; }
    var safeBreakdown = [];
    if (modern) {
      var defs = def.key === "transactions" ? summaryMeasures.slice(0, 4) : summaryMeasures.slice(4);
      if (breakdown.length !== defs.length) { return bad; }
      total = 0;
      for (var j = 0; j < defs.length; j++) {
        var b = breakdown[j], d = defs[j];
        if (!b || b.dimension !== d[0] || b.unit !== d[1] || !summaryNumber(b.total)) { return bad; }
        if (def.key === "memories" || j < 2) { total += b.total; if (!summaryNumber(total)) { return bad; } }
        safeBreakdown.push({ label: d[2], total: b.total });
      }
      if (total !== a.total) { return bad; }
    }
    return { status: "available", bins: a.bins.slice(), total: a.total, unit: def.unit, breakdown: safeBreakdown,
      coverage: modern ? { tracking: summaryDate(coverage.tracking_since), partial: coverage.partial_first_bucket } : null };
  }
  function summaryStateLabel(status) {
    return ({ not_tracked: "Not tracked yet", server_update_needed: "Server update needed", unavailable: "Unavailable", disabled: "Disabled" })[status] || "Unavailable";
  }
  function summaryBreakdown(act, def) {
    if (!act.coverage) { return ""; }
    return '<details class="summary-details" id="summary-details-' + def.key + '"><summary id="summary-disclosure-' + def.key + '" aria-label="' + def.name + ' coverage and components">Coverage and components</summary><span class="summary-breakdown">' + act.breakdown.map(function (b) {
      return '<span>' + b.label + ': ' + b.total + '</span>';
    }).join('') + '</span><small class="summary-coverage">Recorded portion of this window; tracking since ' + act.coverage.tracking +
      (act.coverage.partial ? '; first tracked hour partial' : '') + '; current hour partial. Earlier bins unknown; older clients may omit activity.</small></details>';
  }
  function summaryName(def) {
    return '<span class="summary-name"><b>' + def.code + '</b> ' + def.name + '</span>';
  }
  function summaryInventory(inv) {
    if (inv.status === "not_applicable") { return '<span class="summary-muted">activity only</span>'; }
    if (inv.status !== "available") { return '<span class="summary-muted">' + inv.status + '</span>'; }
    return esc(inv.count) + '<small>' + inv.label + '</small>';
  }
  function summaryAmount(act) {
    if (act.status !== "available") { return '<span class="summary-muted">' + summaryStateLabel(act.status) + '</span>'; }
    return '<strong>' + esc(act.total) + '</strong><small>' + act.unit + (act.coverage ? ' · partial coverage' : '') + '</small>';
  }
  function summaryPattern(act, timeline) {
    if (act.status !== "available") { return '<span class="summary-muted">' + summaryStateLabel(act.status) + '</span>'; }
    var max = Math.max.apply(null, act.bins), chars = "▁▂▃▄▅▆▇█";
    return '<span class="summary-pattern" role="img" aria-label="Hourly quantities, oldest first: ' + act.bins.map(function (n) { return n === null ? "unknown" : n; }).join(", ") + '">' +
      act.bins.map(function (n) {
        var glyph = n === null ? "?" : timeline ? (n === 0 ? "." : n <= 2 ? "░" : n <= 5 ? "▒" : n <= 9 ? "▓" : "█") :
          (n === 0 ? "·" : chars[Math.max(0, Math.ceil(n / max * 8) - 1)]);
        return '<span aria-hidden="true">' + glyph + '</span>';
      }).join("") + '</span>';
  }
  function renderSummary() {
    if (!summaryState.active || !$("summary-body")) { return; }
    var report = summaryState.report, stale = report && (summaryState.failed || Date.now() - Date.parse(report.generated) > 60000);
    var freshness = report ?
      (summaryState.failed ? "Refresh failed · stale report · retry in 30 seconds" : stale ? "Stale report" : "Recorded snapshot") :
      (summaryState.failed ? "Summary unavailable · refresh failed · retry in 30 seconds" : "Loading summary…");
    // Announce state transitions, not the changing generated timestamp on every poll.
    if ($("summary-freshness").textContent !== freshness) { $("summary-freshness").textContent = freshness; }
    $("summary-generated").textContent = report ? ' · Generated ' + report.generated : '';
    $("summary-status").classList.toggle("summary-stale", !!stale || summaryState.failed);
    if (!report) { $("summary-body").innerHTML = '<p class="summary-muted">No report available.</p>'; return; }
    var activeID = document.activeElement && document.activeElement.id;
    var openDetails = {};
    $("summary-body").querySelectorAll(".summary-details").forEach(function (node) { openDetails[node.id] = node.open; });
    var timeline = summaryState.view === "timeline", html;
    if (summaryState.view === "recent") {
      html = '<h3>Updates from loaded records</h3><p class="summary-muted">Latest 12 observations from loaded first pages of up to 100 records per source; not a complete audit log.</p>' +
        (report.recent.length ? report.recent.map(function (r) {
          var def = summaryCategories.filter(function (c) { return c.key === r.key; })[0];
          return '<div class="summary-update summary-' + def.key + '"><time datetime="' + r.at + '">' + r.at.slice(0, 19).replace("T", " ") + ' UTC' + '</time>' + summaryName(def) + '<span>' + r.action + '</span></div>';
        }).join("") : '<p class="summary-muted">No recognized updates in loaded records.</p>');
    } else {
      html = '<div class="summary-columns' + (timeline ? ' summary-timeline' : '') + '" aria-hidden="true"><span>Category</span>' +
        (timeline ? '' : '<span>Inventory</span>') + '<span>Hourly pattern</span><span>Measured quantity</span></div>';
      if (timeline) {
        html += '<p class="summary-muted">' + report.since.slice(0, 16).replace("T", " ") + ' → ' + report.until.slice(0, 16).replace("T", " ") + ' UTC</p>';
      }
      html += report.categories.map(function (c) {
        var def = c.def, tag = def.route ? 'a' : 'div';
        return '<' + tag + ' id="summary-row-' + def.key + '" class="summary-row summary-' + def.key + (timeline ? ' summary-timeline' : '') + '"' +
          (def.route ? ' href="#/' + def.route + '"' : '') + '>' + summaryName(def) +
          (timeline ? '' : '<span class="summary-inventory">' + summaryInventory(c.inventory) + '</span>') +
          summaryPattern(c.activity, timeline) + '<span class="summary-amount">' + summaryAmount(c.activity) + '</span>' + '</' + tag + '>' + summaryBreakdown(c.activity, def);
      }).join("");
      html += '<p class="summary-muted">' + (timeline ? '? unknown · . 0 · ░ 1–2 · ▒ 3–5 · ▓ 6–9 · █ 10+ per hour; each row names its measure.' :
        'Patterns scaled per row; not a volume comparison. ? unknown; current hour partial. Bounded recent records are not inventory totals.') + '</p>';
      html += '<p class="summary-muted">Operations count recorded reads and writes; records have separate quantities. Memory changes count created, revised, archived, restored and deleted records/versions. No combined activity total.</p>';
    }
    $("summary-body").innerHTML = html;
    Object.keys(openDetails).forEach(function (id) { if ($(id)) { $(id).open = openDetails[id]; } });
    if (activeID && /^(summary-row-|summary-disclosure-)/.test(activeID) && $(activeID)) { $(activeID).focus({ preventScroll: true }); }
    $("summary-checkpoints").innerHTML = report.checkpoints.map(function (c) {
      return '<span class="summary-checkpoint"><span>' + c.label + '</span> <b>' + c.status + '</b></span>';
    }).join("");
  }
  function stopSummary() {
    summaryState.active = false;
    summaryState.generation++;
    if (summaryState.timer !== null) { clearTimeout(summaryState.timer); summaryState.timer = null; }
    if (summaryState.deadline !== null) { clearTimeout(summaryState.deadline); summaryState.deadline = null; }
    if (summaryState.controller) { summaryState.controller.abort(); summaryState.controller = null; }
    summaryState.request = null;
  }
  function scheduleSummary() {
    if (!summaryState.active || document.hidden || summaryState.request || summaryState.timer !== null) { return; }
    summaryState.timer = setTimeout(function () {
      summaryState.timer = null;
      refreshSummary();
    }, Math.max(0, summaryState.nextAt - Date.now()));
  }
  function refreshSummary() {
    if (!summaryState.active || document.hidden || summaryState.request) { return; }
    if (Date.now() < summaryState.nextAt) { scheduleSummary(); return; }
    var generation = summaryState.generation;
    summaryState.nextAt = Date.now() + summaryInterval;
    var controller = new AbortController();
    summaryState.controller = controller;
    var timeout = new Promise(function (_, reject) {
      summaryState.deadline = setTimeout(function () {
        controller.abort();
        reject(new Error("Summary timeout"));
      }, 15000);
    });
    var responseBody = fetch("/api/summary", { credentials: "same-origin", signal: controller.signal })
      .then(function (response) {
        if (!response.ok) { throw new Error("Summary unavailable"); }
        return response.json();
      });
    summaryState.request = Promise.race([responseBody, timeout]).then(function (body) {
        if (!summaryState.active || generation !== summaryState.generation) { return; }
        summaryState.report = normalizeSummary(body);
        summaryState.failed = false;
        renderSummary();
      }).catch(function () {
        if (!summaryState.active || generation !== summaryState.generation) { return; }
        summaryState.failed = true;
        renderSummary();
      }).finally(function () {
        if (!summaryState.active || generation !== summaryState.generation) { return; }
        if (summaryState.deadline !== null) { clearTimeout(summaryState.deadline); summaryState.deadline = null; }
        summaryState.request = null;
        summaryState.controller = null;
        // Slow or failed requests never create a rapid retry loop.
        summaryState.nextAt = Date.now() + summaryInterval;
        scheduleSummary();
      });
  }
  function summaryVisibilityChanged() {
    if (!summaryState.active) { return; }
    if (document.hidden) {
      if (summaryState.timer !== null) { clearTimeout(summaryState.timer); summaryState.timer = null; }
    } else { renderSummary(); refreshSummary(); }
  }
  function mountOverview() {
    $("view").innerHTML = '<section class="agent-summary" aria-label="Agent summary"><h2>Agent summary</h2>' +
      '<p class="summary-muted">Recorded activity · 24 hourly buckets · UTC · current hour partial · cache 30s</p>' +
      '<div class="summary-views" role="group" aria-label="Summary view">' +
      [["overview", "Overview"], ["timeline", "Timeline"], ["recent", "Recent updates"]].map(function (pair) {
        return '<button type="button" id="summary-view-' + pair[0] + '" aria-pressed="' + (summaryState.view === pair[0]) + '">' + pair[1] + '</button>';
      }).join("") + '</div><p id="summary-status"><span id="summary-freshness" role="status" aria-atomic="true"></span><span id="summary-generated"></span></p><div id="summary-body"></div>' +
      '<div id="summary-checkpoints" aria-label="Checkpoints"></div></section>' +
      '<details id="workspace-details"><summary>Workspace details</summary><div id="workspace-content"></div></details>' +
      '<div id="overview-self-status" role="status"></div>';
    ["overview", "timeline", "recent"].forEach(function (view) {
      $("summary-view-" + view).addEventListener("click", function () {
        summaryState.view = view;
        ["overview", "timeline", "recent"].forEach(function (name) {
          $("summary-view-" + name).setAttribute("aria-pressed", String(name === view));
        });
        renderSummary();
      });
    });
    $("workspace-details").addEventListener("toggle", function () {
      // Native toggle events are queued and may arrive after navigation.
      if (this !== $("workspace-details")) { return; }
      if (this.open && state.self) { renderOverview(state.self); }
      else { $("workspace-content").innerHTML = ""; }
    });
    summaryState.active = true;
    summaryState.nextAt = 0;
    renderSummary();
    refreshSummary();
  }

  function renderOverview(self) {
    var details = $("workspace-details");
    if ($("overview-self-status")) { $("overview-self-status").textContent = ""; }
    if (!details || !details.open) { return; }
    var focused = document.activeElement;
    var focusedHref = focused && focused.closest && focused.closest("#workspace-content") ? focused.getAttribute("href") : null;
    var counts = (self.index && self.index.counts) || {};
    var cards = Object.keys(counts).sort().map(function (key) {
      var card = '<div class="card"><div class="num">' + esc(counts[key]) + '</div><div class="label">' + esc(key) + "</div></div>";
      if (key === "facts") { return '<a class="card-link" href="#/facts">' + card + "</a>"; }
      if (key === "memories") { return '<a class="card-link" href="#/memories">' + card + "</a>"; }
      if (key === "secrets") { return '<a class="card-link" href="#/secrets">' + card + "</a>"; }
      return card;
    }).join("");
    var salient = (self.salient_memories || []).map(function (memory) {
      return '<div class="row"><span class="grow"><a href="#/memories/' + esc(memory.id) + '">' + esc(memory.snippet || memory.id) + "</a></span>" +
        '<span class="dim">' + esc(memory.kind || "") + '</span><span class="dim mono">' + esc((memory.salience != null ? memory.salience.toFixed(2) : "")) + "</span></div>";
    }).join("");
    var checkpoints = [];
    if (self.memory_checkpoint && self.memory_checkpoint.pending) { checkpoints.push({ label: "memory curation pending" }); }
    if (self.message_checkpoint && self.message_checkpoint.pending) { checkpoints.push({ label: "messaging work pending" }); }
    if (self.email_checkpoint && self.email_checkpoint.pending) { checkpoints.push({ label: "email pending", href: "#/email" }); }
    if (self.avatar_checkpoint && self.avatar_checkpoint.pending) { checkpoints.push({ label: "avatar lifecycle pending" }); }
    $("workspace-content").innerHTML =
      '<div class="panel"><h2>Inventory</h2><div class="cards">' + (cards || '<span class="empty">no counts</span>') + "</div></div>" +
      planEntitlementsHTML(self.plan_entitlements) +
      factCapacityHTML(self.fact_capacity) +
      memoryCapacityHTML(self.memory_capacity) +
      '<div class="panel"><h2>Salient memories</h2><div class="list">' + (salient || '<div class="empty">none</div>') + "</div></div>" +
      '<div class="panel"><h2>Checkpoints</h2><div class="list">' +
      (checkpoints.length ? checkpoints.map(function (item) {
        var label = item.href ? '<a href="' + esc(item.href) + '">' + esc(item.label) + "</a>" : esc(item.label);
        return '<div class="row"><span class="grow">' + label + "</span></div>";
      }).join("") : '<div class="empty">nothing pending</div>') +
      "</div></div>" +
      '<div class="panel"><h2>Reads</h2><div class="dim">' +
      (self.observational === false ? "cell has no observational hooks; plain reads in use" : "observational reads only — viewing never records usage") +
      "</div></div>";
    if (focusedHref !== null) {
      var replacement = Array.prototype.find.call($("workspace-content").querySelectorAll("a"), function (link) {
        return link.getAttribute("href") === focusedHref;
      });
      (replacement || details.querySelector("summary")).focus({ preventScroll: true });
    }
  }

  function viewOverview() {
    var generation = overviewViewGeneration;
    breadcrumb([{ label: "overview" }]);
    mountOverview();
    openEvents(null);
    var selfFrame = state.lastSelfData;
    fetchJSON("/api/self").then(function (self) {
      if (generation !== overviewViewGeneration || selfFrame !== state.lastSelfData) { return; }
      renderHeader(self);
      renderOverview(self);
    }).catch(function (err) {
      if (generation === overviewViewGeneration && selfFrame === state.lastSelfData) {
        $("overview-self-status").textContent = "Workspace details unavailable · self refresh failed";
      }
    });
  }

  // Only capture's documented string fields become inventory state. Paths are
  // reduced before retention; titles are opaque text, never a metadata source.
  function transcriptText(value) {
    if (typeof value !== "string") { return ""; }
    var out = "", mode = 0;
    for (var ch of value) {
      var code = ch.codePointAt(0);
      if (mode === 1) {
        if (ch === "[") { mode = 2; }
        else if ("]PX^_".indexOf(ch) >= 0) { mode = 3; }
        else if (code >= 0x30 && code <= 0x7e) { mode = 0; }
        continue;
      }
      if (mode === 2) { if (code >= 0x40 && code <= 0x7e) { mode = 0; } continue; }
      if (mode === 3) {
        if (code === 7 || code === 0x9c) { mode = 0; }
        else if (code === 27) { mode = 4; }
        continue;
      }
      if (mode === 4) {
        if (ch === "\\" || code === 7 || code === 0x9c) { mode = 0; }
        else if (code !== 27) { mode = 3; }
        continue;
      }
      if (code === 27) { mode = 1; continue; }
      if (code === 0x9b) { mode = 2; continue; }
      if ([0x90, 0x98, 0x9d, 0x9e, 0x9f].indexOf(code) >= 0) { mode = 3; continue; }
      if (ch === "\n" || ch === "\t" || ch === "\r") { out += " "; continue; }
      if (code < 32 || code >= 0x7f && code <= 0x9f || code === 0x061c ||
          code === 0x200e || code === 0x200f || code >= 0x202a && code <= 0x202e ||
          code >= 0x2066 && code <= 0x206f) { continue; }
      out += ch;
    }
    return out.trim();
  }
  function transcriptObject(value) { return value && typeof value === "object" && !Array.isArray(value) ? value : {}; }
  function transcriptWorkspace(value) {
    var path = transcriptText(value).replace(/\\/g, "/").replace(/\/+$/, "");
    if (path.indexOf("//") === 0 && path.slice(2).split("/").filter(Boolean).length <= 2) { return ""; }
    var base = path.slice(path.lastIndexOf("/") + 1);
    return /^[a-z]:$/i.test(base) || base === "." || base === ".." ? "" : base;
  }
  function transcriptRuntime(value) {
    var names = { codex: "Codex", claude: "Claude Code", "claude-code": "Claude Code", claude_code: "Claude Code",
      gemini: "Gemini CLI", "gemini-cli": "Gemini CLI", dsh: "DSH", cursor: "Cursor", copilot: "Copilot", grok: "Grok", "grok-build": "Grok Build", openclaw: "OpenClaw", antigravity: "Antigravity" };
    return Object.prototype.hasOwnProperty.call(names, value.toLowerCase()) ? names[value.toLowerCase()] : value;
  }
  function transcriptProjection(value) {
    var t = transcriptObject(value), md = transcriptObject(t.metadata), location = transcriptObject(md.location);
    var id = transcriptText(t.id);
    return { id: id === t.id ? id : "", title: transcriptText(t.title), externalID: transcriptText(t.external_id),
      created: transcriptText(t.created_at), updated: transcriptText(t.updated_at),
      agent: transcriptText(md.agent_name), client: transcriptRuntime(transcriptText(md.runtime)),
      location: transcriptText(location.name) || transcriptText(location.id), workspace: transcriptWorkspace(md.initial_cwd) };
  }
  var transcriptColumnKeys = ["agent", "location", "client", "workspace", "updated"];
  var transcriptColumnLabels = ["Agent", "Location", "AI client", "Workspace", "Updated"];
  var selectedTranscriptID = ""; // Identity only; metadata lives in this visit's DOM handlers.
  function transcriptValue(value) { return value || "Not recorded"; }
  function transcriptMetadataHTML(t) {
    var keys = transcriptColumnKeys.concat(["created", "title", "externalID", "id"]);
    var labels = transcriptColumnLabels.concat(["Created", "Original title", "External ID", "Transcript ID"]);
    return '<dl class="transcript-metadata">' + keys.map(function (key, index) {
      return '<dt>' + labels[index] + '</dt><dd>' + esc(transcriptValue(t[key])) + '</dd>';
    }).join("") + '</dl>';
  }
  function viewTranscripts() {
    var generation = transcriptViewGeneration;
    breadcrumb([{ label: "transcripts" }]);
    openEvents(null);
    $("view").innerHTML = '<div class="panel"><h2>Transcripts</h2><div role="status">Loading transcripts…</div></div>';
    fetchJSON("/api/transcripts").then(function (body) {
      if (generation !== transcriptViewGeneration) { return; }
      var transcripts = (Array.isArray(body.transcripts) ? body.transcripts : []).map(transcriptProjection);
      var rows = transcripts.map(function (t, index) {
        return '<tr class="transcript-row" role="row" id="transcript-row-' + index + '">' + transcriptColumnKeys.map(function (key, col) {
          var value = esc(transcriptValue(t[key]));
          if (col === 0) { value = '<button type="button" class="transcript-select" id="transcript-select-' + index + '" aria-pressed="false" aria-label="Select session ' + esc(transcriptValue(t.agent) + ': ' + (t.title || t.id || String(index + 1))) + '">' + value + '</button>'; }
          return '<td role="cell"><span class="transcript-mobile-label" aria-hidden="true">' + transcriptColumnLabels[col] + '</span>' + value + '</td>';
        }).join("") + '</tr>';
      }).join("");
      $("view").innerHTML = '<div class="panel transcript-inventory"><h2>Transcripts</h2>' + filterInputHTML("transcripts") +
        (rows ? '<table class="transcript-table" role="table"><caption>Recorded sessions · select a row for details</caption><thead role="rowgroup"><tr role="row">' +
          transcriptColumnLabels.map(function (label) { return '<th scope="col" role="columnheader">' + label + '</th>'; }).join("") +
          '</tr></thead><tbody role="rowgroup">' + rows + '</tbody></table>' : '<div class="empty">no transcripts</div>') +
        '<section id="transcript-selection" class="transcript-selection" aria-label="Selected session" hidden></section></div>';
      var visible = [];
      function select(index) {
        if (generation !== transcriptViewGeneration) { return; }
        selectedTranscriptID = index >= 0 ? transcripts[index].id : "";
        transcripts.forEach(function (_, i) {
          $("transcript-row-" + i).classList.toggle("selected", i === index);
          $("transcript-select-" + i).setAttribute("aria-pressed", String(i === index));
        });
        var details = $("transcript-selection");
        details.hidden = index < 0;
        details.innerHTML = index < 0 ? "" : '<div class="transcript-selection-head"><h3>Selected session</h3>' +
          (transcripts[index].id ? '<a class="transcript-open" href="#/transcripts/' + esc(encodeURIComponent(transcripts[index].id)) + '">Open transcript →</a>' : '') +
          '</div>' + transcriptMetadataHTML(transcripts[index]);
      }
      var input = $("filter-transcripts");
      function filter() {
        if (generation !== transcriptViewGeneration) { return; }
        state.filters.transcripts = input.value;
        var query = input.value.toLowerCase();
        visible = [];
        transcripts.forEach(function (t, i) {
          var corpus = transcriptColumnKeys.map(function (key) { return transcriptValue(t[key]); }).concat([t.title, t.id, t.externalID]).join(" ").toLowerCase();
          var match = !query || corpus.indexOf(query) >= 0;
          $("transcript-row-" + i).style.display = match ? "" : "none";
          if (match) { visible.push(i); }
        });
        $("filter-empty-transcripts").hidden = !query || !transcripts.length || visible.length > 0;
        var selected = visible.find(function (i) { return transcripts[i].id === selectedTranscriptID; });
        select(selected === undefined ? (visible.length ? visible[0] : -1) : selected);
      }
      transcripts.forEach(function (_, i) {
        $("transcript-row-" + i).addEventListener("click", function () { select(i); });
        $("transcript-select-" + i).addEventListener("keydown", function (event) {
          if (generation !== transcriptViewGeneration || (event.key !== "ArrowDown" && event.key !== "ArrowUp")) { return; }
          event.preventDefault();
          var next = visible[Math.max(0, Math.min(visible.length - 1, visible.indexOf(i) + (event.key === "ArrowDown" ? 1 : -1)))];
          if (next !== undefined) { select(next); $("transcript-select-" + next).focus(); }
        });
      });
      input.addEventListener("input", filter);
      $("clear-filter-transcripts").addEventListener("click", function () { if (generation !== transcriptViewGeneration) { return; } input.value = ""; filter(); input.focus(); });
      filter();
    }).catch(function (err) {
      if (generation === transcriptViewGeneration) { showError(err); }
    });
  }

  // parseJSONObjectBody returns the parsed object only when body is a JSON
  // object literal. The leading-character scan keeps the common prose path
  // parse-free, so long tails (hundreds of entries) stay cheap.
  function parseJSONObjectBody(body) {
    if (typeof body !== "string") { return null; }
    var start = 0;
    while (start < body.length && " \t\r\n".indexOf(body.charAt(start)) >= 0) { start++; }
    if (body.charAt(start) !== "{") { return null; }
    var parsed;
    try { parsed = JSON.parse(body); } catch (_) { return null; }
    return parsed && typeof parsed === "object" && !Array.isArray(parsed) ? parsed : null;
  }

  // Bodies that parse as a JSON object (tool calls, structured events) render
  // as a compact head — the tool_name badge when present, else the object's
  // first keys — plus the pretty-printed JSON behind a native, CSP-safe
  // <details> disclosure. Every fragment passes esc() before embedding,
  // including the <summary> content; non-JSON bodies render as before.
  function entryBodyHTML(entry) {
    var body = entry.body || (entry.payload ? "[payload]" : "");
    var parsed = parseJSONObjectBody(body);
    if (!parsed) { return esc(body); }
    var head;
    if (typeof parsed.tool_name === "string" && parsed.tool_name) {
      head = '<span class="tool-badge">' + esc(parsed.tool_name) + "</span>";
    } else {
      var keys = Object.keys(parsed);
      head = '<span class="json-keys">{' +
        esc(keys.slice(0, 3).join(", ") + (keys.length > 3 ? ", \u2026" : "")) + "}</span>";
    }
    return '<details class="entry-json"><summary>' + head + "</summary>" +
      '<pre class="json-body">' + esc(JSON.stringify(parsed, null, 2)) + "</pre></details>";
  }

  function entryHTML(entry, anchored) {
    var role = (entry.role || "").toLowerCase().replace(/[^a-z]/g, "") || "unknown";
    return '<div class="entry role-' + esc(role) + (anchored ? " anchored" : "") + '" data-seq="' + esc(entry.sequence) + '">' +
      '<span class="seq">' + esc(entry.sequence) + "</span>" +
      '<span class="role">' + esc(entry.role || "?") + "</span>" +
      '<span class="body">' + entryBodyHTML(entry) + "</span></div>";
  }

  function appendEntries(transcriptID, entries) {
    var container = $("entries");
    if (!container || container.getAttribute("data-transcript") !== transcriptID) { return; }
    var highest = state.seenSequences[transcriptID] || 0;
    var added = false;
    entries.forEach(function (entry) {
      if (entry.sequence <= highest) { return; }
      container.insertAdjacentHTML("beforeend", entryHTML(entry, false));
      highest = entry.sequence;
      added = true;
    });
    state.seenSequences[transcriptID] = highest;
    if (added) { container.scrollTop = container.scrollHeight; }
  }

  function viewTranscript(id, query) {
    var generation = transcriptViewGeneration;
    breadcrumb([{ label: "transcripts", href: "#/transcripts" }, { label: id }]);
    var from = parseInt(query.from, 10) || 0;
    var until = parseInt(query.to, 10) || from;
    var path = "/api/transcripts/" + encodeURIComponent(id);
    path += from > 0 ? "?after_sequence=" + Math.max(0, from - 1) + "&limit=500" : "?tail=true&limit=200";
    fetchJSON(path).then(function (page) {
      if (generation !== transcriptViewGeneration) { return; }
      var entries = page.entries || [];
      var highest = 0;
      var rows = entries.map(function (entry) {
        if (entry.sequence > highest) { highest = entry.sequence; }
        return entryHTML(entry, from > 0 && entry.sequence >= from && entry.sequence <= until);
      }).join("");
      state.seenSequences[id] = highest;
      var metadata = transcriptProjection(page.transcript);
      var title = metadata.title || metadata.id || id;
      metadata.id = metadata.id || id;
      $("view").innerHTML = '<div class="panel"><h2>' + esc(title) + ' <span class="badge">live tail</span></h2>' +
        '<details class="transcript-reader-metadata"><summary>Session details</summary>' + transcriptMetadataHTML(metadata) + '</details>' +
        '<div id="entries" class="entries" data-transcript="' + esc(id) + '">' +
        (rows || '<div class="empty">no entries yet</div>') + "</div></div>";
      var anchor = document.querySelector(".entry.anchored");
      if (anchor) { anchor.scrollIntoView({ block: "center" }); }
      openEvents(id, highest);
    }).catch(function (err) {
      if (generation === transcriptViewGeneration) { showError(err); }
    });
  }

  // --- facts ------------------------------------------------------------
  // The atomic/semantic plane, rendered from the redacted observational list
  // (/api/facts). Sensitive rows show a lock plus an eye button; clicking the
  // eye fetches the single-fact reveal endpoint — the only response that may
  // carry a sensitive value. Revealed values live only in the replaced DOM
  // subtree: never in state.facts, localStorage, or sessionStorage, and any
  // re-render, navigation, or SSE-driven list refresh discards them.
  var EYE_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" ' +
    'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
    '<path d="M1 12s4-7 11-7 11 7 11 7-4 7-11 7S1 12 1 12z"/><circle cx="12" cy="12" r="3"/></svg>';
  var EYE_SLASH_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" ' +
    'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
    '<path d="M1 12s4-7 11-7 11 7 11 7-4 7-11 7S1 12 1 12z"/><circle cx="12" cy="12" r="3"/>' +
    '<line x1="4" y1="20" x2="20" y2="4"/></svg>';
  var COPY_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" ' +
    'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
    '<rect x="9" y="9" width="12" height="12" rx="2"/>' +
    '<path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';

  function mergeFacts(list) {
    state.facts = {};
    (list || []).forEach(function (fact) {
      if (fact && fact.id) { state.facts[fact.id] = fact; }
    });
  }

  function factValueText(value) {
    if (value == null) { return ""; }
    return typeof value === "string" ? value : JSON.stringify(value);
  }

  function copyButtonHTML(subject, predicate) {
    return '<button class="eye-btn copy-btn" type="button" title="copy value without revealing"' +
      ' aria-label="copy value without revealing"' +
      ' data-subject="' + esc(subject) + '" data-predicate="' + esc(predicate) + '">' + COPY_SVG + "<span>Copy</span></button>";
  }

  function lockedValueHTML(subject, predicate) {
    return '<span class="lock-chip">locked</span>' +
      '<button class="eye-btn" type="button" title="reveal sensitive value" aria-label="reveal sensitive value"' +
      ' data-subject="' + esc(subject) + '" data-predicate="' + esc(predicate) + '">' + EYE_SVG + "<span>Reveal</span></button>" +
      copyButtonHTML(subject, predicate);
  }

  function revealedValueHTML(subject, predicate, value) {
    return '<span class="value">' + esc(factValueText(value)) + "</span>" +
      '<button class="eye-btn" type="button" title="hide value" aria-label="hide value" data-shown="true"' +
      ' data-subject="' + esc(subject) + '" data-predicate="' + esc(predicate) + '">' + EYE_SLASH_SVG + "<span>Hide</span></button>" +
      copyButtonHTML(subject, predicate);
  }

  function factValueHTML(fact) {
    if (fact.sensitive) { return lockedValueHTML(fact.subject, fact.predicate); }
    return '<span class="value">' + esc(factValueText(fact.value)) + "</span>";
  }

  function renderFactsList(facts) {
    var rows = (facts || []).map(function (fact) {
      return '<div class="row"><span class="grow"><a href="#/facts/' + esc(fact.id) + '">' +
        esc(fact.subject) + " \u00b7 " + esc(fact.predicate) + "</a></span>" +
        '<span class="fact-value mono">' + factValueHTML(fact) + "</span>" +
        '<span class="dim">' + esc(fact.source_kind || "") + "</span>" +
        '<span class="dim">' + esc((fact.updated_at || "").slice(0, 19)) + "</span></div>";
    }).join("");
    var caret = captureFilterFocus("facts");
    $("view").innerHTML = '<div class="panel"><h2>Facts</h2>' + filterInputHTML("facts") +
      '<div class="list">' + (rows || '<div class="empty">no facts</div>') + "</div></div>";
    bindFilter("facts");
    restoreFilterFocus("facts", caret);
  }

  function viewFacts() {
    var generation = factViewGeneration;
    breadcrumb([{ label: "facts" }]);
    openEvents(null, 0, false, false, true);
    fetchJSON("/api/facts?limit=100").then(function (body) {
      if (generation !== factViewGeneration) { return; }
      mergeFacts(body.facts);
      renderFactsList(body.facts || []);
    }).catch(function (err) {
      if (generation === factViewGeneration) { showError(err); }
    });
  }

  function viewFact(id) {
    var generation = factViewGeneration;
    breadcrumb([{ label: "facts", href: "#/facts" }, { label: id }]);
    openEvents(null);
    var load = state.facts[id] ? Promise.resolve() :
      fetchJSON("/api/facts?limit=100").then(function (body) {
        if (generation !== factViewGeneration) { return; }
        mergeFacts(body.facts);
      });
    load.then(function () {
      if (generation !== factViewGeneration) { return; }
      var fact = state.facts[id];
      if (!fact) { throw new Error("fact " + id + " is not in the redacted inventory"); }
      // subject/predicate let the proxy prove the fact non-sensitive before
      // forwarding assertion values; without proof it locks the history.
      return fetchJSON("/api/facts/" + encodeURIComponent(id) + "/history?subject=" +
        encodeURIComponent(fact.subject) + "&predicate=" + encodeURIComponent(fact.predicate))
        .then(function (body) {
          if (generation !== factViewGeneration) { return; }
          renderFact(fact, body.assertions || [], body.truncated === true);
        });
    }).catch(function (err) {
      if (generation === factViewGeneration) { showError(err); }
    });
  }

  function renderFact(fact, assertions, truncated) {
    var history = assertions.map(function (assertion) {
      var value = fact.sensitive ? '<span class="lock-chip">locked</span>' :
        '<span class="mono">' + esc(factValueText(assertion.value)) + "</span>";
      return '<div class="row"><span class="grow">' + value + "</span>" +
        '<span class="dim">' + esc(assertion.source_kind || "") + "</span>" +
        '<span class="dim mono">' + esc(assertion.confidence != null ? assertion.confidence.toFixed(2) : "") + "</span>" +
        '<span class="dim">' + esc((assertion.observed_at || "").slice(0, 19)) + "</span></div>";
    }).join("");
    $("view").innerHTML =
      '<div class="panel"><h2>' + esc(fact.subject) + " \u00b7 " + esc(fact.predicate) + "</h2>" +
      '<div class="fact-value fact-detail-value mono">' + factValueHTML(fact) + "</div></div>" +
      '<div class="panel"><h2>Details</h2><dl class="kv">' +
      "<dt>id</dt><dd>" + esc(fact.id) + "</dd>" +
      "<dt>value type</dt><dd>" + esc(fact.value_type || "") + "</dd>" +
      "<dt>cardinality</dt><dd>" + esc(fact.cardinality || "") + "</dd>" +
      "<dt>source</dt><dd>" + esc(fact.source_kind || "") + (fact.source_ref ? " (" + esc(fact.source_ref) + ")" : "") + "</dd>" +
      "<dt>confidence</dt><dd>" + esc(fact.confidence != null ? fact.confidence.toFixed(2) : "") + "</dd>" +
      "<dt>sensitive</dt><dd>" + esc(fact.sensitive ? "yes" : "no") + "</dd>" +
      "<dt>usage</dt><dd>" + esc(fact.usage_count != null ? fact.usage_count : "") + "</dd>" +
      "<dt>updated</dt><dd>" + esc((fact.updated_at || "").slice(0, 19)) + "</dd>" +
      "</dl></div>" +
      '<div class="panel"><h2>Assertion history</h2>' +
      (truncated ? '<div class="fact-note">History truncated: showing the newest 1,000 assertions.</div>' : '') +
      (fact.sensitive ? '<div class="fact-note">sensitive history values stay locked in v1 &mdash; no per-assertion reveal.</div>' : "") +
      '<div class="list">' + (history || '<div class="empty">no assertions</div>') + "</div></div>";
  }

  // Copy-without-reveal: the value goes fetch response -> clipboard and is
  // dropped; it never enters the DOM. The system clipboard is a wider surface
  // than this page (other apps, history tools, Universal Clipboard sync), so
  // a best-effort timed clear follows — blind overwrite, Chromium-only in
  // practice, since checking first would need a clipboard-read prompt.
  var copyClearTimer = null;

  function copyFactValue(subject, predicate) {
    if (!navigator.clipboard || !navigator.clipboard.writeText) {
      toast("clipboard unavailable in this browser");
      return;
    }
    fetchJSON("/api/fact?subject=" + encodeURIComponent(subject) + "&predicate=" + encodeURIComponent(predicate))
      .then(function (body) {
        return navigator.clipboard.writeText(factValueText((body.fact || {}).value));
      })
      .then(function () {
        toast("copied — clipboard may sync to other devices; best-effort clear in 45s");
        if (copyClearTimer) { clearTimeout(copyClearTimer); }
        copyClearTimer = setTimeout(function () {
          copyClearTimer = null;
          navigator.clipboard.writeText("").catch(function () {});
        }, 45000);
      })
      .catch(function (err) { toast("copy failed: " + (err.message || err)); });
  }

  // Delegated eye/copy handler: reveal fetches the single-fact endpoint on
  // the user's click and swaps the value into this DOM subtree only; hide
  // swaps the lock back, dropping the value with the replaced nodes.
  function onRevealClick(event) {
    var target = event.target;
    var button = target && target.closest ? target.closest(".eye-btn") : null;
    if (!button) { return; }
    var subject = button.getAttribute("data-subject");
    var predicate = button.getAttribute("data-predicate");
    if (button.classList.contains("copy-btn")) {
      copyFactValue(subject, predicate);
      return;
    }
    var holder = button.closest(".fact-value");
    if (!holder) { return; }
    if (button.getAttribute("data-shown") === "true") {
      holder.innerHTML = lockedValueHTML(subject, predicate);
      return;
    }
    // Query-addressed like the upstream exact read: a path shape would let
    // the /api/facts/{id}/history route shadow a predicate named "history".
    fetchJSON("/api/fact?subject=" + encodeURIComponent(subject) + "&predicate=" + encodeURIComponent(predicate))
      .then(function (body) {
        holder.innerHTML = revealedValueHTML(subject, predicate, (body.fact || {}).value);
      })
      .catch(function (err) {
        holder.innerHTML = lockedValueHTML(subject, predicate) +
          '<span class="reveal-error">' + esc(err.message || err) + "</span>";
      });
  }

  function renderMemoriesList(page) {
    focusState("memories").page = page;
    var rows = (page.items || []).map(function (memory) {
      var label = (memory.redacted || memory.sensitive) ? "[redacted " + (memory.kind || "memory") + "]" : (memory.content || memory.id);
      return '<div class="row"><span class="grow">' + focusControl("memories", memory.id, label) + "</span>" +
        '<span class="dim">' + esc(memory.kind || "") + "</span>" +
        '<span class="dim">' + esc(memory.state || "") + "</span>" +
        '<span class="dim mono">' + esc(memory.salience != null ? memory.salience.toFixed(2) : "") + "</span></div>";
    }).join("");
    renderFocusList("memories", '<div class="panel"><h2>Memories</h2>' + filterInputHTML("memories") +
      '<div class="list">' + (rows || '<div class="empty">no memories</div>') + "</div></div>");
  }

  function viewMemories() {
    var generation = memoryViewGeneration;
    breadcrumb([{ label: "memories" }]);
    openEvents(null, 0, false, true);
    if (focusState("memories").page) { renderMemoriesList(focusState("memories").page); return; }
    return fetchJSON("/api/memories?limit=100").then(function (page) {
      if (generation !== memoryViewGeneration) { return; }
      renderMemoriesList(page);
    }).catch(function (err) {
      if (generation === memoryViewGeneration) { showError(err); }
    });
  }

  function evidenceHTML(evidence) {
    return (evidence || []).map(function (item) {
      if (item.transcript_id) {
        var from = item.entry_from_sequence || 0;
        var until = item.entry_until_sequence || from;
        var href = "#/transcripts/" + encodeURIComponent(item.transcript_id) +
          (from ? "?from=" + from + "&to=" + until : "");
        return '<div class="row"><span class="grow"><a href="' + esc(href) + '">' + esc(item.transcript_id) +
          (from ? " #" + esc(from) + "&ndash;" + esc(until) : "") + "</a></span>" +
          '<span class="dim">' + esc(item.role || "") + "</span></div>";
      }
      var label = item.source_memory_id || item.message_id || item.import_artifact_id || item.external_locator || item.id;
      return '<div class="row"><span class="grow mono">' + esc(label) + '</span><span class="dim">' + esc(item.role || "") + "</span></div>";
    }).join("");
  }

  function viewMemory(id) {
    var generation = memoryViewGeneration;
    breadcrumb([{ label: "memories", href: "#/memories" }, { label: id }]);
    openEvents(null);
    renderFocusDetail("memories", id, id, '<div class="panel" role="status">Loading memory…</div>');
    return Promise.all([
      fetchJSON("/api/memories/" + encodeURIComponent(id)),
      fetchJSON("/api/memories/" + encodeURIComponent(id) + "/history?limit=50"),
    ]).then(function (results) {
      if (generation !== memoryViewGeneration) { return; }
      var memory = results[0].memory || {};
      var versions = results[1].versions || [];
      var content = memory.redacted ? "[sensitive value redacted]" : (memory.content || "");
      var tags = (memory.tags || []).map(function (tag) { return '<span class="tag">' + esc(tag) + "</span>"; }).join("");
      var history = versions.map(function (version) {
        return '<div class="row"><span class="dim mono">v' + esc(version.version) + "</span>" +
          '<span class="grow">' + esc(version.operation || "") + "</span>" +
          '<span class="dim">' + esc(version.state || "") + "</span>" +
          '<span class="dim">' + esc((version.created_at || "").slice(0, 19)) + "</span></div>";
      }).join("");
      renderFocusDetail("memories", id, id,
        '<div class="panel"><h2>Memory ' + esc(id) + '</h2><div class="memory-content">' + esc(content) + "</div>" +
        '<div class="tags">' + tags + "</div></div>" +
        '<div class="panel"><h2>Details</h2><dl class="kv">' +
        "<dt>kind</dt><dd>" + esc(memory.kind || "") + "</dd>" +
        "<dt>state</dt><dd>" + esc(memory.state || "") + "</dd>" +
        "<dt>salience</dt><dd>" + esc(memory.salience != null ? memory.salience.toFixed(2) : "") + "</dd>" +
        "<dt>version</dt><dd>" + esc(memory.version || "") + "</dd>" +
        "<dt>origin</dt><dd>" + esc(memory.origin || "") + "</dd>" +
        "<dt>sensitive</dt><dd>" + esc(memory.sensitive ? "yes" : "no") + "</dd>" +
        "</dl></div>" +
        '<div class="panel"><h2>Evidence</h2><div class="list">' +
        (evidenceHTML(memory.evidence) || '<div class="empty">no evidence rows</div>') + "</div></div>" +
        '<div class="panel"><h2>Version history</h2><div class="list">' +
        (history || '<div class="empty">no versions</div>') + "</div></div>", false);
      focusElement($("focus-detail"));
    }).catch(function (err) {
      if (generation === memoryViewGeneration) { renderFocusDetail("memories", id, id, '<div class="error">' + esc(err.message || err) + "</div>"); }
    });
  }

  // --- secrets ----------------------------------------------------------
  // The sealed plane, rendered strictly from proxy-sanitized metadata
  // (/api/secrets). There is no eye icon and no copy here by design: the
  // vault key is client custody, the backend stores ciphertext only, and
  // the proxy strips every value slot — so unlike facts, there is nothing a
  // reveal could fetch without shipping secret material to a browser.
  var SECRETS_NOTE = '<div class="secret-note">sealed values are client-custodied; ' +
    "the backend stores ciphertext only and this dashboard never renders secret material.</div>";

  function renderSecretsUnavailable() {
    $("view").innerHTML = '<div class="panel"><h2>Secrets</h2>' +
      '<div class="empty">sealed plane not available on this cell</div>' +
      SECRETS_NOTE + "</div>";
  }

  function secretsError(err) {
    if (err && err.status === 501) { renderSecretsUnavailable(); return; }
    showError(err);
  }

  function secretFieldsLabel(secret) {
    var total = secret.field_count != null ? secret.field_count : (secret.fields || []).length;
    var sensitive = secret.sensitive_field_count || 0;
    return total + " field" + (total === 1 ? "" : "s") + ", " + sensitive + " sensitive";
  }

  function renderSecretsList(secrets) {
    var rows = (secrets || []).map(function (secret) {
      return '<div class="row"><span class="grow"><a href="#/secrets/' + esc(secret.id) + '">' +
        esc(secret.name || secret.id) + "</a></span>" +
        '<span class="dim">' + esc(secretFieldsLabel(secret)) + "</span>" +
        '<span class="dim">' + esc(secret.lifecycle || "") + "</span>" +
        '<span class="dim">' + esc((secret.updated_at || "").slice(0, 19)) + "</span></div>";
    }).join("");
    var caret = captureFilterFocus("secrets");
    $("view").innerHTML = '<div class="panel"><h2>Secrets <span class="badge">metadata only</span></h2>' +
      SECRETS_NOTE + filterInputHTML("secrets") + '<div class="list">' +
      (rows || '<div class="empty">no secrets</div>') + "</div></div>";
    bindFilter("secrets");
    restoreFilterFocus("secrets", caret);
  }

  function viewSecrets() {
    var generation = secretViewGeneration;
    breadcrumb([{ label: "secrets" }]);
    openEvents(null, 0, false, false, false, true);
    fetchJSON("/api/secrets?limit=100").then(function (body) {
      if (generation !== secretViewGeneration) { return; }
      renderSecretsList(body.secrets || []);
    }).catch(function (err) { if (generation === secretViewGeneration) { secretsError(err); } });
  }

  function secretFieldRow(field) {
    return '<div class="row"><span class="grow">' + esc(field.name || field.id) + "</span>" +
      '<span class="dim">' + esc(field.kind || "") + "</span>" +
      (field.sensitive ? '<span class="lock-chip">sensitive</span>' : "") + "</div>";
  }

  function viewSecret(id) {
    var generation = secretViewGeneration;
    breadcrumb([{ label: "secrets", href: "#/secrets" }, { label: id }]);
    openEvents(null);
    fetchJSON("/api/secrets/" + encodeURIComponent(id)).then(function (body) {
      if (generation !== secretViewGeneration) { return; }
      var secret = body.secret || {};
      var vaultKey = body.vault_key || {};
      var fields = (secret.fields || []).map(secretFieldRow).join("");
      var binding = vaultKey.id ?
        vaultKey.id + (vaultKey.key_version != null ? " (v" + vaultKey.key_version + ")" : "") : "—";
      $("view").innerHTML =
        '<div class="panel"><h2>' + esc(secret.name || secret.id) + ' <span class="badge">metadata only</span></h2>' +
        SECRETS_NOTE + "</div>" +
        '<div class="panel"><h2>Details</h2><dl class="kv">' +
        "<dt>id</dt><dd>" + esc(secret.id) + "</dd>" +
        "<dt>state</dt><dd>" + esc(secret.lifecycle || "") + "</dd>" +
        "<dt>created</dt><dd>" + esc((secret.created_at || "").slice(0, 19)) + "</dd>" +
        "<dt>updated</dt><dd>" + esc((secret.updated_at || "").slice(0, 19)) + "</dd>" +
        "<dt>fields</dt><dd>" + esc(secret.field_count != null ? secret.field_count : "") + "</dd>" +
        "<dt>sensitive fields</dt><dd>" + esc(secret.sensitive_field_count != null ? secret.sensitive_field_count : "") + "</dd>" +
        "<dt>vault-key binding</dt><dd>" + esc(binding) + "</dd>" +
        "</dl></div>" +
        '<div class="panel"><h2>Fields</h2><div class="list">' +
        (fields || '<div class="empty">no fields</div>') + "</div></div>";
    }).catch(function (err) { if (generation === secretViewGeneration) { secretsError(err); } });
  }

  // --- read-only agent email ------------------------------------------
  // Received and sent email are separate purpose-built projections. Neither
  // contains email/message ids, bodies, raw MIME/header material, attachment
  // details, worker fences, or action capabilities. The pane never composes,
  // replies, acknowledges, marks read, retries, or changes email settings.
  function emailQuery() {
    var params = ["limit=100"];
    if (state.emailFilters.unread) { params.push("unread=true"); }
    if (state.emailFilters.unacked) { params.push("unacked=true"); }
    return params.join("&");
  }

  function normalizeEmailUnavailableReason(err) {
    var message = String((err && err.message) || err || "").toLowerCase();
    if (message.indexOf("not enabled on this account") >= 0 ||
        message.indexOf("feature_not_enabled") >= 0 ||
        message === "feature_disabled") { return "feature_disabled"; }
    if ((err && (err.status === 404 || err.status === 501)) ||
        message.indexOf("not implemented") >= 0 ||
        message.indexOf("pre_feature") >= 0 ||
        message.indexOf("pre-feature") >= 0) { return "pre_feature"; }
    if ((err && (err.status === 502 || err.status === 503 || err.status === 504)) ||
        message.indexOf("backend_unavailable") >= 0 ||
        message.indexOf("upstream") >= 0) { return "upstream"; }
    if ((err && err.status === 403) || message.indexOf("not enrolled") >= 0 ||
        message === "not_enrolled") { return "not_enrolled"; }
    return "unavailable";
  }

  function emailUnavailableReason(err) { return normalizeEmailUnavailableReason(err); }

  function emailAvailabilityLabel(reason) {
    reason = normalizeEmailUnavailableReason(reason);
    if (reason === "feature_disabled") { return "feature disabled"; }
    if (reason === "not_enrolled") { return "not enrolled"; }
    if (reason === "pre_feature") { return "not available yet"; }
    if (reason === "upstream") { return "upstream degraded"; }
    return "unavailable";
  }

  function emailAvailabilityMessage(direction, reason) {
    reason = normalizeEmailUnavailableReason(reason);
    if (reason === "feature_disabled") {
      return direction === "sent"
        ? "outbound email is not enabled on this account. Sent history remains subscribed and will recover after a live policy change; no reinstall is required."
        : "inbound email is not enabled on this account. This pane will reprobe after a live policy change; no reinstall is required.";
    }
    if (reason === "not_enrolled") {
      return direction === "sent"
        ? "this agent is not enrolled in outbound email."
        : "this agent is not enrolled in inbound email.";
    }
    if (reason === "pre_feature") {
      return direction === "sent"
        ? "sent email history is not available in this cell build."
        : "received email is not available in this cell build.";
    }
    if (reason === "upstream") {
      return direction === "sent"
        ? "sent email history is temporarily unavailable because its upstream service is degraded."
        : "received email is temporarily unavailable because its upstream service is degraded.";
    }
    return direction === "sent"
      ? "sent email history is temporarily unavailable on this cell."
      : "received email is temporarily unavailable on this cell.";
  }

  function emailUnavailablePanelHTML(direction, reason, trailingHTML) {
    var heading = direction === "sent" ? "Sent email" : "Received email";
    return '<div class="panel"><h2>' + heading + ' <span class="badge">' +
      esc(emailAvailabilityLabel(reason)) + '</span></h2><div class="empty">' +
      esc(emailAvailabilityMessage(direction, reason)) + "</div>" + (trailingHTML || "") + "</div>";
  }

  function emailLoadingPanelHTML(direction) {
    var heading = direction === "sent" ? "Sent email" : "Received email";
    return '<div class="panel"><h2>' + heading +
      ' <span class="badge">read only</span></h2><div class="empty">checking availability&hellip;</div></div>';
  }

  function renderEmailUnavailable(reason) {
    state.emailAvailable = false;
    state.emailReceiveUnavailableReason = normalizeEmailUnavailableReason(reason);
    renderEmailList();
  }

  function emailSignals(message) {
    var values = [];
    [["spf", message.spf_result], ["dkim", message.dkim_result], ["dmarc", message.dmarc_result],
      ["spam", message.spam_verdict]].forEach(function (pair) {
      if (pair[1]) { values.push(pair[0] + " " + pair[1]); }
    });
    return values.join(" · ");
  }

  function formatEmailBytes(value) {
    value = Math.max(0, Number(value) || 0);
    if (value < 1024) { return value + " B"; }
    if (value < 1024 * 1024) { return (value / 1024).toFixed(1) + " KiB"; }
    if (value < 1024 * 1024 * 1024) { return (value / (1024 * 1024)).toFixed(1) + " MiB"; }
    return (value / (1024 * 1024 * 1024)).toFixed(1) + " GiB";
  }

  function emailStorageStatusHTML(status) {
    if (!status) { return ""; }
    var maximumRaw = Math.max(0, Number(status.maximum_raw_bytes) || 0);
    var capacity = status.attachment_capacity || {};
    var used = Math.max(0, Number(capacity.used) || 0);
    var level = capacity.over_limit || capacity.at_limit ? "danger" : (capacity.near_limit ? "warning" : "");
    var stateLabel = capacity.over_limit ? "over limit" :
      (capacity.at_limit ? "at limit" : (capacity.near_limit ? "near limit" : "available"));
    var header = '<div class="email-note">maximum raw message size: ' +
      esc(formatEmailBytes(maximumRaw)) + "</div>";
    if (capacity.unlimited) {
      return '<div class="panel"><h2>Email storage</h2>' + header +
        '<div class="capacity-line"><span>account-wide attachment capacity: ' +
        esc(formatEmailBytes(used)) + ' used</span><span class="badge">unlimited</span></div></div>';
    }
    var maximum = Math.max(0, Number(capacity.max) || 0);
    var remaining = Math.max(0, Number(capacity.remaining) || 0);
    return '<div class="panel capacity ' + level + '"><h2>Email storage</h2>' + header +
      '<div class="capacity-line"><span>account-wide attachment capacity: ' +
      esc(formatEmailBytes(used)) + " of " + esc(formatEmailBytes(maximum)) +
      ' used</span><span class="badge">' + esc(stateLabel) + "</span></div>" +
      '<progress class="capacity-track" aria-label="account-wide attachment capacity" max="' +
      esc(maximum || 1) + '" value="' + esc(Math.min(used, maximum || 1)) + '"></progress>' +
      '<div class="dim">' + esc(formatEmailBytes(remaining)) + " remaining</div></div>";
  }

  function emailPayloadRetentionWarning(message) {
    return message && message.payload_retention_state === "omitted_capacity"
      ? "attachment payload omitted because account-wide capacity is full"
      : "";
  }

  function receivedEmailHTML() {
    if (state.emailAvailable === null) { return emailLoadingPanelHTML("received"); }
    if (state.emailAvailable !== true) {
      return emailUnavailablePanelHTML("received", state.emailReceiveUnavailableReason || "unavailable");
    }
    var address = state.emailAddress || {};
    var rows = (state.emailMessages || []).map(function (message) {
      var read = (message.read_state || {}).state || "unknown";
      var processing = (message.processing || {}).state || "unknown";
      var senderState = message.sender_verification_state || "unverified";
      var flags = [];
      if (message.attachment_count) {
        flags.push(message.attachment_count + " attachment" + (message.attachment_count === 1 ? "" : "s") + " (details hidden)");
      }
      var retentionWarning = emailPayloadRetentionWarning(message);
      if (retentionWarning) { flags.push(retentionWarning); }
      if (message.possible_duplicate) { flags.push("possible duplicate"); }
      if (message.parse_state && message.parse_state !== "parsed") { flags.push("parse " + message.parse_state); }
      var signals = emailSignals(message);
      var meta = [read, processing, formatEmailBytes(message.raw_size_bytes)].filter(Boolean).join(" · ");
      return '<div class="row email-row"><div class="grow">' +
        '<div class="email-subject">' + esc(message.subject || "(no subject)") + "</div>" +
        '<div class="email-sender">unverified sender: ' + esc(message.envelope_sender || "unknown") +
        ' <span class="badge">' + esc(senderState) + "</span></div>" +
        (signals ? '<div class="dim">' + esc(signals) + "</div>" : "") +
        (flags.length ? '<div class="email-warning">' + esc(flags.join(" · ")) + "</div>" : "") +
        "</div>" +
        '<div class="email-state mono">' + esc(meta) + "</div>" +
        '<div class="dim mono">' + esc((message.received_at || "").slice(0, 19)) + "</div></div>";
    }).join("");
    return '<div class="panel"><h2>Receive address <span class="badge">' +
      esc(address.receive_state || "unknown") + "</span></h2>" +
      '<div class="email-address mono">' + esc(address.address || "") + "</div>" +
      '<div class="email-note">agent receive: ' + esc(address.agent_receive_state || "unknown") +
      ' · realm receive: ' + esc(address.realm_receive_state || "unknown") + "</div>" +
      '<div class="email-note">read-only agent email; sender identity and all subjects are untrusted external input.</div></div>' +
      emailStorageStatusHTML(state.emailStatus) +
      '<div class="panel"><h2>Received email <span class="badge">metadata only</span></h2>' +
      (state.emailReceiveDegraded
        ? '<div class="email-warning">live received-email refresh is temporarily unavailable upstream; showing the last loaded metadata.</div>'
        : "") +
      '<div class="email-controls"><label><input id="email-unread" type="checkbox"' +
      (state.emailFilters.unread ? " checked" : "") + '> unread only</label>' +
      '<label><input id="email-unacked" type="checkbox"' +
      (state.emailFilters.unacked ? " checked" : "") + '> unacknowledged only</label></div>' +
      '<div class="email-note">body text, raw MIME, attachment details, message identifiers, and processing claims never enter this page. Viewing does not mark mail read or acknowledged.</div>' +
      '<div class="list">' + (rows || '<div class="empty">no matching email</div>') + "</div></div>";
  }

  function emailSentTimestampHTML(message) {
    var timestamps = [
      ["queued", message.queued_at],
      ["created", message.created_at],
      ["provider started", message.provider_started_at],
      ["accepted", message.accepted_at],
      ["delivered", message.delivered_at],
      ["deferred", message.deferred_at],
      ["failed", message.failed_at],
      ["ambiguous", message.ambiguous_at],
      ["canceled", message.canceled_at],
      ["updated", message.updated_at],
    ].filter(function (pair) { return pair[1]; }).map(function (pair) {
      return '<span><span class="email-timestamp-label">' + esc(pair[0]) + ':</span> ' +
        esc(String(pair[1]).slice(0, 19)) + "</span>";
    }).join("");
    return '<div class="email-timestamps mono">' +
      (timestamps || '<span><span class="email-timestamp-label">timestamps:</span> not reported</span>') +
      "</div>";
  }

  function emailSentRowsHTML(messages) {
    return (messages || []).map(function (message) {
      message = message || {};
      var attempts = Number(message.attempt_count);
      attempts = !isFinite(attempts) || attempts < 0 ? 0 : Math.floor(attempts);
      var stateLabel = message.state || "unknown";
      var providerState = message.provider_state || "not reported";
      var errorCode = message.error_code || "none";
      var requestKind = message.request_kind || "not reported";
      return '<div class="row email-row email-sent-row"><div class="grow">' +
        '<div class="email-subject">' + esc(message.subject || "(no subject)") + "</div>" +
        '<div class="email-recipient">recipient: <span class="mono">' + esc(message.to || "not reported") + "</span></div>" +
        '<div class="email-route">from: <span class="mono">' + esc(message.from || "not reported") +
        '</span> · reply-to: <span class="mono">' + esc(message.reply_to || "not reported") + "</span></div>" +
        '<div class="email-lifecycle mono">kind: ' + esc(requestKind) +
        " · provider state: " + esc(providerState) +
        " · error code: " + esc(errorCode) + " · attempts: " + esc(attempts) + "</div>" +
        emailSentTimestampHTML(message) + "</div>" +
        '<div class="email-state"><span class="badge">state: ' + esc(stateLabel) + "</span></div></div>";
    }).join("");
  }

  function sentEmailHTML() {
    if (state.emailSentAvailable === null) { return emailLoadingPanelHTML("sent"); }
    var rows = emailSentRowsHTML(state.emailSentMessages);
    var newestNote = '<div class="email-note">newest 100 sent messages at most; older sent messages are not loaded. Bodies, message identifiers, and delivery actions never enter this page.</div>';
    if (state.emailSentAvailable !== true) {
      var retained = rows
        ? newestNote + '<div class="email-warning">showing the last loaded sent metadata while live refresh is unavailable.</div><div class="list">' + rows + "</div>"
        : "";
      return emailUnavailablePanelHTML("sent", state.emailSentUnavailableReason || "unavailable", retained);
    }
    return '<div class="panel"><h2>Sent email <span class="badge">metadata only</span></h2>' +
      newestNote + (state.emailSentDegraded
        ? '<div class="email-warning">live sent-email refresh is temporarily unavailable upstream; showing the last loaded metadata.</div>'
        : "") + '<div class="list">' + (rows || '<div class="empty">no sent email</div>') + "</div></div>";
  }

  function renderEmailList() {
    $("view").innerHTML = receivedEmailHTML() + sentEmailHTML();
    bindEmailControls();
  }

  function invalidateEmailView() {
    state.emailViewGeneration++;
    return state.emailViewGeneration;
  }

  function emailViewIsCurrent(viewGeneration) {
    return viewGeneration === state.emailViewGeneration && parseHash().section === "email";
  }

  function currentEmailViewGeneration(viewGeneration) {
    return typeof viewGeneration === "number" ? viewGeneration : state.emailViewGeneration;
  }

  function emailReceiveShouldPoll() {
    if (state.emailAvailable === true) { return true; }
    if (state.emailAvailable !== false) { return false; }
    var reason = normalizeEmailUnavailableReason(state.emailReceiveUnavailableReason);
    return reason === "upstream" || reason === "unavailable";
  }

  function settleReceivedEmailUnavailable(reason, clearAddress) {
    reason = normalizeEmailUnavailableReason(reason);
    var transientFailure = reason === "upstream" || reason === "unavailable";
    state.emailAvailable = false;
    state.emailReceiveUnavailableReason = reason;
    state.emailReceiveDegraded = transientFailure;
    // A transient list/status/address miss does not invalidate an address we
    // already displayed. Settled policy/enrollment/build states do.
    if (clearAddress && !transientFailure) { state.emailAddress = null; }
    if (!transientFailure) { state.emailAddressRecoveryPending = false; }
    state.emailStatus = null;
    state.emailMessages = [];
    if (reason === "feature_disabled") {
      state.emailCheckpointEnabled = false;
      // A stale disabled response can arrive after an enabled self frame.
      // Permit the next identical frame to be processed so it can trigger
      // the one-shot mailbox reprobe.
      state.lastSelfData = null;
    }
  }

  function openEmailEvents(viewGeneration) {
    viewGeneration = currentEmailViewGeneration(viewGeneration);
    if (!emailViewIsCurrent(viewGeneration)) { return; }
    openEvents(null, 0, false, false, false, false, emailReceiveShouldPoll(),
      state.emailFilters.unread, state.emailFilters.unacked, true);
  }

  function retryEmailAddressAfterRecovery(viewGeneration) {
    if (!state.emailAddressRecoveryPending || state.emailAddress ||
        !emailViewIsCurrent(viewGeneration)) { return null; }
    state.emailAddressRecoveryPending = false;
    return probeEmailMailbox(viewGeneration, true);
  }

  function refreshEmail(viewGeneration) {
    viewGeneration = currentEmailViewGeneration(viewGeneration);
    var liveRevision = state.emailReceiveLiveRevision;
    var requestRevision = ++state.emailReceiveRequestRevision;
    return Promise.all([
      fetchJSON("/api/email/status"),
      fetchJSON("/api/email?" + emailQuery()),
    ]).then(function (results) {
      if (!emailViewIsCurrent(viewGeneration) ||
          liveRevision !== state.emailReceiveLiveRevision ||
          requestRevision !== state.emailReceiveRequestRevision) { return; }
      var statusBody = results[0];
      var body = results[1];
      if (body.available === false) {
        settleReceivedEmailUnavailable(body.reason || "unavailable", true);
        renderEmailUnavailable(state.emailReceiveUnavailableReason);
        openEmailEvents(viewGeneration);
        return;
      }
      state.emailStatus = statusBody.status || null;
      state.emailAvailable = true;
      state.emailReceiveUnavailableReason = null;
      state.emailReceiveDegraded = false;
      state.emailMessages = body.messages || [];
      renderEmailList();
      openEmailEvents(viewGeneration);
    }).catch(function (err) {
      if (!emailViewIsCurrent(viewGeneration) ||
          liveRevision !== state.emailReceiveLiveRevision ||
          requestRevision !== state.emailReceiveRequestRevision) { return; }
      settleReceivedEmailUnavailable(emailUnavailableReason(err), true);
      // Transient direct-read failures deliberately keep the receive poll in
      // the live stream. A later successful frame heals the pane without a
      // reload; settled policy/enrollment/build states remain sent-only.
      openEmailEvents(viewGeneration);
      renderEmailUnavailable(state.emailReceiveUnavailableReason);
    });
  }

  function settleSentEmailUnavailable(reason) {
    reason = normalizeEmailUnavailableReason(reason);
    state.emailSentAvailable = false;
    state.emailSentUnavailableReason = reason;
    state.emailSentDegraded = reason === "upstream" || reason === "unavailable";
    // A transient upstream failure may recover with the next SSE poll. Keep
    // the last loaded projection clearly labeled; settled policy/build
    // unavailability clears it so stale lifecycle is never mistaken as live.
    if (reason !== "upstream" && reason !== "unavailable") { state.emailSentMessages = []; }
  }

  function refreshSentEmail(viewGeneration) {
    viewGeneration = currentEmailViewGeneration(viewGeneration);
    var liveRevision = state.emailSentLiveRevision;
    return fetchJSON("/api/email/sent?limit=100").then(function (body) {
      if (!emailViewIsCurrent(viewGeneration) ||
          liveRevision !== state.emailSentLiveRevision) { return; }
      if (body.available === false) {
        settleSentEmailUnavailable(body.reason || "unavailable");
      } else {
        state.emailSentAvailable = true;
        state.emailSentUnavailableReason = null;
        state.emailSentDegraded = false;
        state.emailSentMessages = body.messages || [];
      }
      renderEmailList();
    }).catch(function (err) {
      if (!emailViewIsCurrent(viewGeneration) ||
          liveRevision !== state.emailSentLiveRevision) { return; }
      settleSentEmailUnavailable(emailUnavailableReason(err));
      renderEmailList();
    });
  }

  function bindEmailControls() {
    [["email-unread", "unread"], ["email-unacked", "unacked"]].forEach(function (pair) {
      var input = $(pair[0]);
      if (!input) { return; }
      input.addEventListener("change", function () {
        state.emailFilters[pair[1]] = input.checked;
        refreshEmail(state.emailViewGeneration);
      });
    });
  }

  function viewEmail(viewGeneration) {
    viewGeneration = currentEmailViewGeneration(viewGeneration);
    state.emailAddressRecoveryPending = false;
    breadcrumb([{ label: "agent email" }]);
    // Sent lifecycle polling starts even while inbound enrollment is being
    // resolved. The two availability domains never gate or erase each other.
    renderEmailList();
    openEmailEvents(viewGeneration);
    refreshSentEmail(viewGeneration);
    probeEmailMailbox(viewGeneration);
  }

  // Resolve enrollment after opening the pane and after a live account policy
  // transition from disabled to enabled. The checkpoint edge invokes this
  // once; ordinary enabled checkpoints never create a polling loop.
  function probeEmailMailbox(viewGeneration, recoveryAttempt) {
    viewGeneration = currentEmailViewGeneration(viewGeneration);
    var liveRevision = state.emailReceiveLiveRevision;
    return fetchJSON("/api/email/address").then(function (body) {
      if (!emailViewIsCurrent(viewGeneration)) { return; }
      var receiveChanged = liveRevision !== state.emailReceiveLiveRevision;
      if (receiveChanged && recoveryAttempt !== true) {
        // List/status state may legitimately arrive before the independent
        // address read. Fill an empty address without perturbing that newer
        // content, but never replace an address already advanced elsewhere or
        // reopen a settled disabled/enrollment/build state.
        if (body.available !== false && !state.emailAddress &&
            (state.emailAvailable === true || emailReceiveShouldPoll())) {
          state.emailAddress = body.address || null;
          renderEmailList();
        }
        return;
      }
      // A recovery address read is independent from the list/status frame.
      // Accept only a successful address while receive state is still live or
      // transient; never let this bounded one-shot probe reopen a settled
      // disabled/enrollment/build state or overwrite newer list/status data.
      if (recoveryAttempt === true) {
        state.emailAddressRecoveryPending = false;
        if (body.available === false ||
            (state.emailAvailable !== true && !emailReceiveShouldPoll())) { return; }
        state.emailAddress = body.address || null;
        renderEmailList();
        return;
      }
      if (body.available === false) {
        settleReceivedEmailUnavailable(body.reason || "unavailable", true);
        if (state.emailReceiveDegraded && !state.emailAddress && recoveryAttempt !== true) {
          state.emailAddressRecoveryPending = true;
        }
        openEmailEvents(viewGeneration);
        renderEmailUnavailable(state.emailReceiveUnavailableReason);
        return;
      }
      state.emailAddress = body.address || null;
      state.emailAddressRecoveryPending = false;
      state.emailAvailable = true;
      state.emailReceiveUnavailableReason = null;
      return refreshEmail(viewGeneration);
    }).catch(function (err) {
      if (!emailViewIsCurrent(viewGeneration)) { return; }
      if (recoveryAttempt === true) {
        // Address recovery is independent from list/status availability. One
        // failed bounded probe must not erase a healthy live frame or recurse.
        state.emailAddressRecoveryPending = false;
        return;
      }
      if (liveRevision !== state.emailReceiveLiveRevision) {
        // The live frame that made this direct result stale arrived before we
        // knew address recovery was needed. Arm and run the one bounded probe
        // immediately; identical future frames may be de-duplicated and are
        // therefore not a reliable retry trigger.
        var staleReason = emailUnavailableReason(err);
        if ((staleReason === "upstream" || staleReason === "unavailable") &&
            !state.emailAddress &&
            (state.emailAvailable === true || emailReceiveShouldPoll())) {
          state.emailAddressRecoveryPending = true;
          return retryEmailAddressAfterRecovery(viewGeneration);
        }
        return;
      }
      settleReceivedEmailUnavailable(emailUnavailableReason(err), true);
      if (state.emailReceiveDegraded && !state.emailAddress && recoveryAttempt !== true) {
        state.emailAddressRecoveryPending = true;
      }
      openEmailEvents(viewGeneration);
      renderEmailUnavailable(state.emailReceiveUnavailableReason);
    });
  }

  // --- conversations ----------------------------------------------------
  // Thread-grouped view of the realm-local mailbox built strictly from the
  // passive metadata-only list (/api/messages, upstream GET /v1/messages).
  // The dashboard never calls :read or :listen, and the passive list never
  // carries bodies. A received body is fetched only by its explicit control
  // and lives in the current detail DOM, never in state.messages. The upstream cursor
  // pages only backward in time; live updates re-poll the first page of each
  // direction and this side dedupes by message id.
  function mergeMessages(list, dir) {
    var changed = false;
    (list || []).forEach(function (msg) {
      if (!msg || !msg.id) { return; }
      var key = dir + " " + msg.id;
      var previous = state.messages[key];
      msg._dir = dir;
      if (previous &&
          (previous.read_state || {}).state === (msg.read_state || {}).state &&
          (previous.delivery || {}).state === (msg.delivery || {}).state) { return; }
      state.messages[key] = msg;
      changed = true;
    });
    return changed;
  }

  function fetchMessages() {
    return Promise.all([
      fetchJSON("/api/messages?direction=inbox&limit=100"),
      fetchJSON("/api/messages?direction=outbox&limit=100"),
    ]).then(function (pages) {
      mergeMessages(pages[0].messages, "received");
      mergeMessages(pages[1].messages, "sent");
    });
  }

  // threadKey groups by counterpart: the peer agent for direct messages,
  // while realm-wide broadcasts and multi-agent sends each form their own
  // audience thread (no single stable counterpart id exists for them).
  function threadKey(msg) {
    var audience = (msg.to || {}).kind;
    if (audience === "realm" || audience === "agents") { return audience; }
    var peer = msg._dir === "sent" ? (msg.to || {}) : (msg.from || {});
    return peer.agent_id || "unknown";
  }

  function threadLabel(key, latest) {
    if (key === "realm") { return "realm broadcast"; }
    if (key === "agents") { return "group send"; }
    var peer = latest._dir === "sent" ? (latest.to || {}) : (latest.from || {});
    return peer.agent_name || peer.agent_id || key;
  }

  function buildThreads() {
    var byKey = {};
    Object.keys(state.messages).forEach(function (id) {
      var msg = state.messages[id];
      var key = threadKey(msg);
      var thread = byKey[key] || (byKey[key] = { key: key, messages: [], unread: 0 });
      thread.messages.push(msg);
      if (msg._dir === "received" && (msg.read_state || {}).state === "unread") { thread.unread++; }
    });
    var threads = Object.keys(byKey).map(function (key) {
      var thread = byKey[key];
      thread.messages.sort(function (a, b) {
        if (a.created_at !== b.created_at) { return a.created_at < b.created_at ? -1 : 1; }
        return a.id < b.id ? -1 : (a.id > b.id ? 1 : 0);
      });
      var latest = thread.messages[thread.messages.length - 1];
      thread.label = threadLabel(key, latest);
      thread.latestAt = latest.created_at || "";
      return thread;
    });
    threads.sort(function (a, b) { return a.latestAt < b.latestAt ? 1 : (a.latestAt > b.latestAt ? -1 : 0); });
    return threads;
  }

  function renderConversationList() {
    invalidateMessageBodyView();
    var rows = buildThreads().map(function (thread) {
      return '<div class="row"><span class="grow">' + focusControl("conversations", thread.key, thread.label) +
        (thread.unread ? ' <span class="unread">' + esc(thread.unread) + "</span>" : "") + "</span>" +
        '<span class="dim">' + esc(thread.messages.length) + " msg" + (thread.messages.length === 1 ? "" : "s") + "</span>" +
        '<span class="dim">' + esc(thread.latestAt.slice(0, 19)) + "</span></div>";
    }).join("");
    focusState("conversations").loaded = true;
    renderFocusList("conversations", '<div class="panel"><h2>Conversations</h2>' + filterInputHTML("conversations") +
      '<div class="list">' + (rows || '<div class="empty">no messages</div>') + "</div></div>");
  }

  function bubbleHTML(msg) {
    var to = msg.to || {};
    var head = [];
    if (msg._dir === "received") {
      head.push('<span class="from">' + esc((msg.from || {}).agent_name || (msg.from || {}).agent_id || "?") + "</span>");
    }
    if (to.kind === "realm") { head.push('<span class="dim">to realm</span>'); }
    if (to.kind === "agents") { head.push('<span class="dim">to ' + esc(to.count || 0) + " agents</span>"); }
    if (msg.kind) { head.push('<span class="kind">' + esc(msg.kind) + "</span>"); }
    var meta = [(msg.created_at || "").slice(0, 19)];
    if ((msg.delivery || {}).state) { meta.push(msg.delivery.state); }
    if ((msg.read_state || {}).state) { meta.push(msg.read_state.state); }
    return '<div class="bubble ' + (msg._dir === "sent" ? "sent" : "received") + '">' +
      (head.length ? '<div class="bubble-head">' + head.join(" ") + "</div>" : "") +
      (msg.subject ? '<div class="subject">' + esc(msg.subject) + "</div>" : "") +
      (msg._dir === "received" ? '<div class="message-preview" data-message-id="' + esc(msg.id) + '" data-body-state="hidden">' +
        '<button type="button" class="message-body-toggle" aria-expanded="false">Show body</button>' +
        '<div class="message-body-status nobody" role="status">[body not shown]</div>' +
        '<div class="message-body-content" hidden></div></div>' : '<div class="nobody">[body not shown]</div>') +
      '<div class="meta mono">' + esc(meta.join(" \u00b7 ")) + "</div></div>";
  }

  function renderConversation(key) {
    var view = $("view");
    // Capture geometry before replacing DOM: rebuilding a long thread can
    // clamp its scroll offset even when we do not request tail positioning.
    var documentScroller = document.scrollingElement;
    var viewTop = view.scrollTop;
    var documentTop = documentScroller ? documentScroller.scrollTop : 0;
    var scroller = view.clientHeight >= view.scrollHeight && documentScroller ? documentScroller : view;
    // Match the conversation edge used by scrollIntoView on mobile; the
    // document can continue below it with a wrapped status footer.
    var distanceToBottom = scroller === documentScroller ?
      view.getBoundingClientRect().bottom - documentScroller.clientHeight :
      scroller.scrollHeight - scroller.clientHeight - scroller.scrollTop;
    var nearBottom = distanceToBottom <= 64;
    var previous = Object.create(null);
    if (messageBodyView && messageBodyView.key === key) {
      view.querySelectorAll(".message-preview").forEach(function (node) {
        previous[node.getAttribute("data-message-id")] = node;
      });
    } else {
      invalidateMessageBodyView();
      messageBodyView = { key: key };
    }
    var thread = null;
    buildThreads().forEach(function (candidate) { if (candidate.key === key) { thread = candidate; } });
    // Only identities are retained, never body values or rendered HTML. The
    // owner exists before fetch completes, so an absent ID set means first render.
    var renderedIDs = messageBodyView.renderedIDs;
    var nextIDs = Object.create(null);
    var hasNewMessage = false;
    (thread ? thread.messages : []).forEach(function (msg) {
      var id = msg._dir + " " + msg.id;
      nextIDs[id] = true;
      if (!renderedIDs || !renderedIDs[id]) { hasNewMessage = true; }
    });
    var follow = !renderedIDs || (hasNewMessage && nearBottom);
    var bubbles = thread ? thread.messages.map(bubbleHTML).join("") : "";
    renderFocusDetail("conversations", key, thread ? thread.label : key, '<div class="panel"><h2>' + esc(thread ? thread.label : key) +
      ' <span class="badge">read-only</span></h2>' +
      '<div class="thread-note">Received bodies stay hidden until you choose Show body. Viewing does not mark a message read or acknowledged.</div>' +
      '<div class="bubbles">' + (bubbles || '<div class="empty">no messages in this thread</div>') + "</div></div>", !renderedIDs);
    // Retain only preview nodes still represented in this same detail view.
    // A metadata refresh must neither refetch bodies nor move their values
    // into the metadata cache. Removed nodes are cleared before release.
    view.querySelectorAll(".message-preview").forEach(function (node) {
      var id = node.getAttribute("data-message-id");
      if (previous[id]) {
        node.replaceWith(previous[id]);
        delete previous[id];
      }
    });
    Object.keys(previous).forEach(function (id) { setMessageBodyPreview(previous[id], "hidden"); });
    if (messageBodyPending && !view.contains(messageBodyPending.node)) { cancelMessageBodyRequest(); }
    updateMessageBodyButtons();
    messageBodyView.renderedIDs = nextIDs;
    if (follow) {
      view.scrollTop = view.scrollHeight;
      // Mobile lets the view grow with its contents; check the new layout.
      // Lightweight DOM adapters may omit scrolling methods.
      if (view.clientHeight >= view.scrollHeight && typeof view.scrollIntoView === "function") {
        view.scrollIntoView({ block: "end" });
      }
    } else {
      view.scrollTop = viewTop;
      if (documentScroller) { documentScroller.scrollTop = documentTop; }
    }
  }

  // At most one active body fetch exists. The token holds only request
  // ownership and DOM references, never a retained body or payload cache.
  var messageBodyView = null;
  var messageBodyPending = null;
  var conversationViewSerial = 0;

  function setMessageBodyPreview(node, mode, body) {
    var button = node.querySelector(".message-body-toggle");
    var content = node.querySelector(".message-body-content");
    var status = node.querySelector(".message-body-status");
    node.setAttribute("data-body-state", mode);
    button.textContent = mode === "shown" || mode === "loading" ? "Hide body" : "Show body";
    button.setAttribute("aria-expanded", mode === "shown" || mode === "loading" ? "true" : "false");
    content.textContent = mode === "shown" ? body : "";
    content.hidden = mode !== "shown";
    status.textContent = mode === "loading" ? "Loading body…" :
      (mode === "unavailable" ? "Body unavailable." : (mode === "hidden" ? "[body not shown]" : ""));
  }

  function updateMessageBodyButtons() {
    $("view").querySelectorAll(".message-preview").forEach(function (node) {
      var mode = node.getAttribute("data-body-state");
      node.querySelector(".message-body-toggle").disabled = mode === "unavailable" ||
        (mode === "hidden" && messageBodyPending !== null);
    });
  }

  function cancelMessageBodyRequest() {
    if (!messageBodyPending) { return; }
    var pending = messageBodyPending;
    messageBodyPending = null;
    clearTimeout(pending.timer);
    pending.controller.abort();
  }

  function invalidateMessageBodyView() {
    conversationViewSerial++;
    messageBodyView = null;
    cancelMessageBodyRequest();
    $("view").querySelectorAll(".message-preview").forEach(function (node) {
      setMessageBodyPreview(node, "hidden");
    });
    updateMessageBodyButtons();
  }

  function currentMessageBodyRequest(pending) {
    var current = parseHash();
    return messageBodyPending === pending && messageBodyView === pending.view &&
      current.section === "conversations" && current.id && decodeURIComponent(current.id) === pending.view.key &&
      $("view").contains(pending.node);
  }

  function finishMessageBodyRequest(pending) {
    clearTimeout(pending.timer);
    if (messageBodyPending === pending) { messageBodyPending = null; }
  }

  function onMessageBodyClick(event) {
    var target = event.target;
    var button = target && target.closest ? target.closest(".message-body-toggle") : null;
    if (!button || !$("view").contains(button) || button.disabled || !messageBodyView) { return; }
    var node = button.closest(".message-preview");
    var current = parseHash();
    if (!node || current.section !== "conversations" || !current.id || decodeURIComponent(current.id) !== messageBodyView.key) { return; }
    var id = node.getAttribute("data-message-id");
    // Inbox and outbox copies of the same id remain separate. A sent-only
    // entry can never gain an eligible control through a shared id.
    var metadata = state.messages["received " + id];
    if (!metadata || metadata._dir !== "received" || threadKey(metadata) !== messageBodyView.key) { return; }
    var mode = node.getAttribute("data-body-state");
    if (mode === "shown" || mode === "loading") {
      if (messageBodyPending && messageBodyPending.node === node) { cancelMessageBodyRequest(); }
      setMessageBodyPreview(node, "hidden");
      updateMessageBodyButtons();
      return;
    }
    if (mode !== "hidden" || messageBodyPending) { return; }
    var pending = { node: node, view: messageBodyView, controller: new AbortController(), timer: null };
    messageBodyPending = pending;
    setMessageBodyPreview(node, "loading");
    updateMessageBodyButtons();
    pending.timer = setTimeout(function () {
      var current = currentMessageBodyRequest(pending);
      finishMessageBodyRequest(pending);
      pending.controller.abort();
      if (current) { setMessageBodyPreview(node, "unavailable"); }
      updateMessageBodyButtons();
    }, 10000);
    return fetch("/api/messages/" + encodeURIComponent(id) + "/body", {
      method: "GET", credentials: "same-origin", cache: "no-store", signal: pending.controller.signal,
    }).then(function (response) {
      if (!response.ok) { throw new Error("body unavailable"); }
      return response.json();
    }).then(function (body) {
      if (!currentMessageBodyRequest(pending)) {
        finishMessageBodyRequest(pending);
        updateMessageBodyButtons();
        return;
      }
      if (!body || Array.isArray(body) || Object.keys(body).length !== 1 ||
          typeof body.body !== "string" || new TextEncoder().encode(body.body).length > 65536) {
        throw new Error("body unavailable");
      }
      finishMessageBodyRequest(pending);
      setMessageBodyPreview(node, "shown", body.body);
      updateMessageBodyButtons();
    }).catch(function () {
      if (!currentMessageBodyRequest(pending)) {
        finishMessageBodyRequest(pending);
        updateMessageBodyButtons();
        return;
      }
      finishMessageBodyRequest(pending);
      setMessageBodyPreview(node, "unavailable");
      updateMessageBodyButtons();
    });
  }

  function viewConversations() {
    invalidateMessageBodyView();
    var serial = conversationViewSerial;
    breadcrumb([{ label: "conversations" }]);
    openEvents(null, 0, true);
    if (focusState("conversations").loaded) { renderConversationList(); return; }
    return fetchMessages().then(function () {
      var current = parseHash();
      if (serial === conversationViewSerial && current.section === "conversations" && !current.id) { renderConversationList(); }
    }).catch(function (err) { if (serial === conversationViewSerial) { showError(err); } });
  }

  function viewConversation(key) {
    invalidateMessageBodyView();
    var owner = messageBodyView = { key: key };
    breadcrumb([{ label: "conversations", href: "#/conversations" }, { label: key }]);
    openEvents(null, 0, true);
    if (focusState("conversations").loaded) { renderConversation(key); return; }
    renderFocusDetail("conversations", key, key, '<div class="panel" role="status">Loading conversation…</div>');
    return fetchMessages().then(function () {
      var current = parseHash();
      if (messageBodyView === owner && current.section === "conversations" && current.id && decodeURIComponent(current.id) === key) { focusState("conversations").loaded = true; renderConversation(key); }
    }).catch(function (err) { if (messageBodyView === owner) { renderFocusDetail("conversations", key, key, '<div class="error">' + esc(err.message || err) + '</div>'); } });
  }

  // --- boot -------------------------------------------------------------
  // Keep a narrow CommonJS seam for the direct transition regression test.
  // Browsers do not define module, so the production boot path is unchanged.
  if (typeof module !== "undefined" && module.exports) {
    module.exports = {
      account: { check: checkAccountContext, visibility: accountVisibilityChanged, start: startAccount, stop: stopAccount, money: accountMoneyText },
      normalizeSummary: normalizeSummary,
      summaryState: summaryState,
      refreshSummary: refreshSummary,
      summaryVisibilityChanged: summaryVisibilityChanged,
      state: state,
      route: route,
      mergeMessages: mergeMessages,
      renderConversation: renderConversation,
      onMessageBodyClick: onMessageBodyClick,
      invalidateEmailView: invalidateEmailView,
      probeEmailMailbox: probeEmailMailbox,
      refreshEmail: refreshEmail,
      refreshSentEmail: refreshSentEmail,
      openEmailEvents: openEmailEvents,
      renderEmailList: renderEmailList,
      emailSentRowsHTML: emailSentRowsHTML,
      normalizeEmailUnavailableReason: normalizeEmailUnavailableReason,
      emailStorageStatusHTML: emailStorageStatusHTML,
      emailPayloadRetentionWarning: emailPayloadRetentionWarning,
      factCapacityHTML: factCapacityHTML,
      memoryCapacityHTML: memoryCapacityHTML,
      planEntitlementsHTML: planEntitlementsHTML,
      applyEmailCheckpoint: function (checkpoint) {
        var change = updateEmailAddressFromCheckpoint(checkpoint);
        if (!change || parseHash().section !== "email") { return null; }
        if (change === "disabled") {
          renderEmailUnavailable("feature_disabled");
          return null;
        }
        if (change === "reenabled") { return probeEmailMailbox(); }
        renderEmailList();
        return null;
      },
    };
    return;
  }
  initTheme();
  initAvatarDialog();
  $("status-addr").textContent = window.location.host;
  $("view").addEventListener("click", onRevealClick);
  $("view").addEventListener("click", onMessageBodyClick);
  document.addEventListener("visibilitychange", summaryVisibilityChanged);
  window.addEventListener("hashchange", route);
  startAccount();
  route();
})();
