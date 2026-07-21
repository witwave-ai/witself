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
    sseEmail: false,     // whether the current EventSource polls email metadata
    sseEmailUnread: false,
    sseEmailUnacked: false,
    seenSequences: {},   // transcript id -> highest rendered sequence
    messages: {},        // direction + " " + message id -> passive metadata
    emailAddress: null,  // display-only receive address projection
    emailMessages: [],   // metadata only; no ids, bodies, MIME, or claim fence
    emailAvailable: null,
    emailFilters: { unread: false, unacked: false },
    facts: {},           // fact id -> redacted fact (never a revealed value)
    filters: {},         // section -> list filter text, reapplied on re-render
    upstreamErrors: {},  // SSE source -> upstream error text while degraded
    lastSelfData: null,     // last raw "self" frame, to skip no-op re-renders
    lastMemoriesData: null, // last raw "memories" frame, same reason
    lastFactsData: null,    // last raw "facts" frame, same reason
    lastSecretsData: null,  // last raw "secrets" frame, same reason
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
      ' value="' + esc(state.filters[section] || "") + '">';
  }

  function applyRowFilter(section) {
    var input = $("filter-" + section);
    var panel = input && input.closest ? input.closest(".panel") : null;
    if (!panel) { return; }
    var query = (state.filters[section] || "").toLowerCase();
    panel.querySelectorAll(".row").forEach(function (row) {
      row.style.display = !query || row.textContent.toLowerCase().indexOf(query) >= 0 ? "" : "none";
    });
  }

  function bindFilter(section) {
    var input = $("filter-" + section);
    if (!input) { return; }
    input.addEventListener("input", function () {
      state.filters[section] = input.value;
      applyRowFilter(section);
    });
    applyRowFilter(section);
  }

  // An SSE-driven list re-render replaces the whole panel: the rebuilt input
  // carries the saved filter value but not keyboard focus, so keystrokes
  // landing mid-typing would silently go nowhere. Capture focus and caret
  // before the innerHTML swap and restore them onto the rebuilt input after.
  function captureFilterFocus(section) {
    var active = document.activeElement;
    if (!active || active.id !== "filter-" + section) { return null; }
    return { start: active.selectionStart, end: active.selectionEnd };
  }

  function restoreFilterFocus(section, caret) {
    if (!caret) { return; }
    var input = $("filter-" + section);
    if (!input) { return; }
    input.focus();
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
    if (!state.emailAddress || !checkpoint || checkpoint.unavailable) { return false; }
    var changed = false;
    [["receive_state", checkpoint.receive_state],
      ["agent_receive_state", checkpoint.agent_receive_state],
      ["realm_receive_state", checkpoint.realm_receive_state]].forEach(function (pair) {
      if (typeof pair[1] !== "string" || !pair[1] || state.emailAddress[pair[0]] === pair[1]) { return; }
      state.emailAddress[pair[0]] = pair[1];
      changed = true;
    });
    return changed;
  }

  function setSSEState(up) {
    var dot = $("live-dot");
    dot.classList.toggle("up", up === true);
    dot.classList.toggle("down", up === false);
    $("live-label").textContent = up === true ? "live" : (up === false ? "offline" : "connecting");
    $("status-sse").textContent = "sse " + (up === true ? "connected" : (up === false ? "reconnecting" : "idle"));
  }

  // Upstream degradation from the SSE "upstream" channel: the segment lists
  // the erroring sources while any is down and clears on recovery. Text goes
  // in via textContent/title assignment only, so the server-supplied error
  // text stays inert markup-wise.
  function renderUpstreamStatus() {
    var node = $("status-upstream");
    var sources = Object.keys(state.upstreamErrors).sort();
    if (!sources.length) {
      node.textContent = "";
      node.removeAttribute("title");
      return;
    }
    node.textContent = "upstream degraded: " + sources.join(", ");
    node.title = sources.map(function (source) {
      return source + ": " + state.upstreamErrors[source];
    }).join("\n");
  }

  // --- server-sent events ----------------------------------------------
  function openEvents(transcriptID, afterSequence, withMessages, withMemories, withFacts, withSecrets, withEmail, emailUnread, emailUnacked) {
    withMessages = withMessages === true;
    withMemories = withMemories === true;
    withFacts = withFacts === true;
    withSecrets = withSecrets === true;
    withEmail = withEmail === true;
    emailUnread = withEmail && emailUnread === true;
    emailUnacked = withEmail && emailUnacked === true;
    if (state.eventSource && state.sseTranscript === (transcriptID || null) &&
        state.sseMessages === withMessages && state.sseMemories === withMemories &&
        state.sseFacts === withFacts && state.sseSecrets === withSecrets &&
        state.sseEmail === withEmail && state.sseEmailUnread === emailUnread &&
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
    var source = new EventSource("/api/events" + (params.length ? "?" + params.join("&") : ""));
    state.eventSource = source;
    state.sseTranscript = transcriptID || null;
    state.sseMessages = withMessages;
    state.sseMemories = withMemories;
    state.sseFacts = withFacts;
    state.sseSecrets = withSecrets;
    state.sseEmail = withEmail;
    state.sseEmailUnread = emailUnread;
    state.sseEmailUnacked = emailUnacked;
    // Each stream's tracker starts fresh server-side and re-announces any
    // still-failing source, so stale degradation must not carry over. The
    // same reset runs in onopen: a browser auto-reconnect reuses this
    // EventSource without re-running openEvents, and a source that recovered
    // while disconnected emits no clearing event — the fresh tracker only
    // announces failures — so it would otherwise stay red forever.
    state.upstreamErrors = {};
    renderUpstreamStatus();
    source.onopen = function () {
      setSSEState(true);
      state.upstreamErrors = {};
      renderUpstreamStatus();
    };
    source.onerror = function () { setSSEState(false); };
    source.addEventListener("upstream", function (event) {
      var body;
      try { body = JSON.parse(event.data); } catch (_) { return; }
      if (!body || !body.source) { return; }
      if (body.ok) { delete state.upstreamErrors[body.source]; }
      else { state.upstreamErrors[body.source] = String(body.message || "upstream error"); }
      renderUpstreamStatus();
    });
    source.addEventListener("self", function (event) {
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
      if (emailStateChanged && current.section === "email") { renderEmailList(); }
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
      var body;
      try { body = JSON.parse(event.data); } catch (_) { return; }
      if (body.available === false) {
        state.emailAvailable = false;
        state.emailMessages = [];
        if (parseHash().section === "email") { renderEmailUnavailable(body.reason || "unavailable"); }
        // Keep the general self digest live without repeatedly probing a
        // missing or no-longer-enrolled mailbox every poll interval.
        openEvents(null);
        return;
      }
      state.emailAvailable = true;
      state.emailMessages = body.messages || [];
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

  function route() {
    var route = parseHash();
    setNav(route.section);
    if (route.section === "transcripts" && route.id) { return viewTranscript(route.id, route.query); }
    if (route.section === "transcripts") { return viewTranscripts(); }
    if (route.section === "facts" && route.id) { return viewFact(route.id); }
    if (route.section === "facts") { return viewFacts(); }
    if (route.section === "memories" && route.id) { return viewMemory(route.id); }
    if (route.section === "memories") { return viewMemories(); }
    if (route.section === "secrets" && route.id) { return viewSecret(route.id); }
    if (route.section === "secrets") { return viewSecrets(); }
    if (route.section === "conversations" && route.id) { return viewConversation(decodeURIComponent(route.id)); }
    if (route.section === "conversations") { return viewConversations(); }
    if (route.section === "email") { return viewEmail(); }
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

  // --- views ------------------------------------------------------------
  function renderOverview(self) {
    var counts = (self.index && self.index.counts) || {};
    var cards = Object.keys(counts).sort().map(function (key) {
      var card = '<div class="card"><div class="num">' + esc(counts[key]) + '</div><div class="label">' + esc(key) + "</div></div>";
      if (key === "facts") { return '<a class="card-link" href="#/facts">' + card + "</a>"; }
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
    $("view").innerHTML =
      '<div class="panel"><h2>inventory</h2><div class="cards">' + (cards || '<span class="empty">no counts</span>') + "</div></div>" +
      '<div class="panel"><h2>salient memories</h2><div class="list">' + (salient || '<div class="empty">none</div>') + "</div></div>" +
      '<div class="panel"><h2>checkpoints</h2><div class="list">' +
      (checkpoints.length ? checkpoints.map(function (item) {
        var label = item.href ? '<a href="' + esc(item.href) + '">' + esc(item.label) + "</a>" : esc(item.label);
        return '<div class="row"><span class="grow">' + label + "</span></div>";
      }).join("") : '<div class="empty">nothing pending</div>') +
      "</div></div>" +
      '<div class="panel"><h2>reads</h2><div class="dim">' +
      (self.observational === false ? "cell has no observational hooks; plain reads in use" : "observational reads only — viewing never records usage") +
      "</div></div>";
  }

  function viewOverview() {
    breadcrumb([{ label: "overview" }]);
    openEvents(null);
    fetchJSON("/api/self").then(function (self) {
      renderHeader(self);
      renderOverview(self);
    }).catch(showError);
  }

  function viewTranscripts() {
    breadcrumb([{ label: "transcripts" }]);
    openEvents(null);
    fetchJSON("/api/transcripts").then(function (body) {
      var rows = (body.transcripts || []).map(function (transcript) {
        return '<div class="row"><span class="grow"><a href="#/transcripts/' + esc(transcript.id) + '">' +
          esc(transcript.title || transcript.external_id || transcript.id) + "</a></span>" +
          '<span class="dim mono">' + esc(transcript.id) + '</span>' +
          '<span class="dim">' + esc((transcript.updated_at || "").slice(0, 19)) + "</span></div>";
      }).join("");
      $("view").innerHTML = '<div class="panel"><h2>transcripts</h2>' + filterInputHTML("transcripts") +
        '<div class="list">' + (rows || '<div class="empty">no transcripts</div>') + "</div></div>";
      bindFilter("transcripts");
    }).catch(showError);
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
    breadcrumb([{ label: "transcripts", href: "#/transcripts" }, { label: id }]);
    var from = parseInt(query.from, 10) || 0;
    var until = parseInt(query.to, 10) || from;
    var path = "/api/transcripts/" + encodeURIComponent(id);
    path += from > 0 ? "?after_sequence=" + Math.max(0, from - 1) + "&limit=500" : "?tail=true&limit=200";
    fetchJSON(path).then(function (page) {
      var entries = page.entries || [];
      var highest = 0;
      var rows = entries.map(function (entry) {
        if (entry.sequence > highest) { highest = entry.sequence; }
        return entryHTML(entry, from > 0 && entry.sequence >= from && entry.sequence <= until);
      }).join("");
      state.seenSequences[id] = highest;
      var title = (page.transcript && (page.transcript.title || page.transcript.id)) || id;
      $("view").innerHTML = '<div class="panel"><h2>' + esc(title) + ' <span class="badge">live tail</span></h2>' +
        '<div id="entries" class="entries" data-transcript="' + esc(id) + '">' +
        (rows || '<div class="empty">no entries yet</div>') + "</div></div>";
      var anchor = document.querySelector(".entry.anchored");
      if (anchor) { anchor.scrollIntoView({ block: "center" }); }
      openEvents(id, highest);
    }).catch(showError);
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
      ' data-subject="' + esc(subject) + '" data-predicate="' + esc(predicate) + '">' + COPY_SVG + "</button>";
  }

  function lockedValueHTML(subject, predicate) {
    return '<span class="lock-chip">locked</span>' +
      '<button class="eye-btn" type="button" title="reveal sensitive value" aria-label="reveal sensitive value"' +
      ' data-subject="' + esc(subject) + '" data-predicate="' + esc(predicate) + '">' + EYE_SVG + "</button>" +
      copyButtonHTML(subject, predicate);
  }

  function revealedValueHTML(subject, predicate, value) {
    return '<span class="value">' + esc(factValueText(value)) + "</span>" +
      '<button class="eye-btn" type="button" title="hide value" aria-label="hide value" data-shown="true"' +
      ' data-subject="' + esc(subject) + '" data-predicate="' + esc(predicate) + '">' + EYE_SLASH_SVG + "</button>" +
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
    $("view").innerHTML = '<div class="panel"><h2>facts</h2>' + filterInputHTML("facts") +
      '<div class="list">' + (rows || '<div class="empty">no facts</div>') + "</div></div>";
    bindFilter("facts");
    restoreFilterFocus("facts", caret);
  }

  function viewFacts() {
    breadcrumb([{ label: "facts" }]);
    openEvents(null, 0, false, false, true);
    fetchJSON("/api/facts?limit=100").then(function (body) {
      mergeFacts(body.facts);
      renderFactsList(body.facts || []);
    }).catch(showError);
  }

  function viewFact(id) {
    breadcrumb([{ label: "facts", href: "#/facts" }, { label: id }]);
    openEvents(null);
    var load = state.facts[id] ? Promise.resolve() :
      fetchJSON("/api/facts?limit=100").then(function (body) { mergeFacts(body.facts); });
    load.then(function () {
      var fact = state.facts[id];
      if (!fact) { throw new Error("fact " + id + " is not in the redacted inventory"); }
      // subject/predicate let the proxy prove the fact non-sensitive before
      // forwarding assertion values; without proof it locks the history.
      return fetchJSON("/api/facts/" + encodeURIComponent(id) + "/history?subject=" +
        encodeURIComponent(fact.subject) + "&predicate=" + encodeURIComponent(fact.predicate))
        .then(function (body) { renderFact(fact, body.assertions || []); });
    }).catch(showError);
  }

  function renderFact(fact, assertions) {
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
      '<div class="fact-value mono">' + factValueHTML(fact) + "</div></div>" +
      '<div class="panel"><h2>details</h2><dl class="kv">' +
      "<dt>id</dt><dd>" + esc(fact.id) + "</dd>" +
      "<dt>value type</dt><dd>" + esc(fact.value_type || "") + "</dd>" +
      "<dt>cardinality</dt><dd>" + esc(fact.cardinality || "") + "</dd>" +
      "<dt>source</dt><dd>" + esc(fact.source_kind || "") + (fact.source_ref ? " (" + esc(fact.source_ref) + ")" : "") + "</dd>" +
      "<dt>confidence</dt><dd>" + esc(fact.confidence != null ? fact.confidence.toFixed(2) : "") + "</dd>" +
      "<dt>sensitive</dt><dd>" + esc(fact.sensitive ? "yes" : "no") + "</dd>" +
      "<dt>usage</dt><dd>" + esc(fact.usage_count != null ? fact.usage_count : "") + "</dd>" +
      "<dt>updated</dt><dd>" + esc((fact.updated_at || "").slice(0, 19)) + "</dd>" +
      "</dl></div>" +
      '<div class="panel"><h2>assertion history</h2>' +
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
    var rows = (page.items || []).map(function (memory) {
      var label = memory.redacted ? "[redacted " + (memory.kind || "memory") + "]" : (memory.content || memory.id);
      return '<div class="row"><span class="grow"><a href="#/memories/' + esc(memory.id) + '">' + esc(label) + "</a></span>" +
        '<span class="dim">' + esc(memory.kind || "") + "</span>" +
        '<span class="dim">' + esc(memory.state || "") + "</span>" +
        '<span class="dim mono">' + esc(memory.salience != null ? memory.salience.toFixed(2) : "") + "</span></div>";
    }).join("");
    var caret = captureFilterFocus("memories");
    $("view").innerHTML = '<div class="panel"><h2>memories</h2>' + filterInputHTML("memories") +
      '<div class="list">' + (rows || '<div class="empty">no memories</div>') + "</div></div>";
    bindFilter("memories");
    restoreFilterFocus("memories", caret);
  }

  function viewMemories() {
    breadcrumb([{ label: "memories" }]);
    openEvents(null, 0, false, true);
    fetchJSON("/api/memories?limit=100").then(renderMemoriesList).catch(showError);
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
    breadcrumb([{ label: "memories", href: "#/memories" }, { label: id }]);
    openEvents(null);
    Promise.all([
      fetchJSON("/api/memories/" + encodeURIComponent(id)),
      fetchJSON("/api/memories/" + encodeURIComponent(id) + "/history?limit=50"),
    ]).then(function (results) {
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
      $("view").innerHTML =
        '<div class="panel"><h2>memory ' + esc(id) + '</h2><div class="memory-content">' + esc(content) + "</div>" +
        '<div class="tags">' + tags + "</div></div>" +
        '<div class="panel"><h2>details</h2><dl class="kv">' +
        "<dt>kind</dt><dd>" + esc(memory.kind || "") + "</dd>" +
        "<dt>state</dt><dd>" + esc(memory.state || "") + "</dd>" +
        "<dt>salience</dt><dd>" + esc(memory.salience != null ? memory.salience.toFixed(2) : "") + "</dd>" +
        "<dt>version</dt><dd>" + esc(memory.version || "") + "</dd>" +
        "<dt>origin</dt><dd>" + esc(memory.origin || "") + "</dd>" +
        "<dt>sensitive</dt><dd>" + esc(memory.sensitive ? "yes" : "no") + "</dd>" +
        "</dl></div>" +
        '<div class="panel"><h2>evidence</h2><div class="list">' +
        (evidenceHTML(memory.evidence) || '<div class="empty">no evidence rows</div>') + "</div></div>" +
        '<div class="panel"><h2>version history</h2><div class="list">' +
        (history || '<div class="empty">no versions</div>') + "</div></div>";
    }).catch(showError);
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
    $("view").innerHTML = '<div class="panel"><h2>secrets</h2>' +
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
    $("view").innerHTML = '<div class="panel"><h2>secrets <span class="badge">metadata only</span></h2>' +
      SECRETS_NOTE + filterInputHTML("secrets") + '<div class="list">' +
      (rows || '<div class="empty">no secrets</div>') + "</div></div>";
    bindFilter("secrets");
    restoreFilterFocus("secrets", caret);
  }

  function viewSecrets() {
    breadcrumb([{ label: "secrets" }]);
    openEvents(null, 0, false, false, false, true);
    fetchJSON("/api/secrets?limit=100").then(function (body) {
      renderSecretsList(body.secrets || []);
    }).catch(secretsError);
  }

  function secretFieldRow(field) {
    return '<div class="row"><span class="grow">' + esc(field.name || field.id) + "</span>" +
      '<span class="dim">' + esc(field.kind || "") + "</span>" +
      (field.sensitive ? '<span class="lock-chip">sensitive</span>' : "") + "</div>";
  }

  function viewSecret(id) {
    breadcrumb([{ label: "secrets", href: "#/secrets" }, { label: id }]);
    openEvents(null);
    fetchJSON("/api/secrets/" + encodeURIComponent(id)).then(function (body) {
      var secret = body.secret || {};
      var vaultKey = body.vault_key || {};
      var fields = (secret.fields || []).map(secretFieldRow).join("");
      var binding = vaultKey.id ?
        vaultKey.id + (vaultKey.key_version != null ? " (v" + vaultKey.key_version + ")" : "") : "—";
      $("view").innerHTML =
        '<div class="panel"><h2>' + esc(secret.name || secret.id) + ' <span class="badge">metadata only</span></h2>' +
        SECRETS_NOTE + "</div>" +
        '<div class="panel"><h2>details</h2><dl class="kv">' +
        "<dt>id</dt><dd>" + esc(secret.id) + "</dd>" +
        "<dt>state</dt><dd>" + esc(secret.lifecycle || "") + "</dd>" +
        "<dt>created</dt><dd>" + esc((secret.created_at || "").slice(0, 19)) + "</dd>" +
        "<dt>updated</dt><dd>" + esc((secret.updated_at || "").slice(0, 19)) + "</dd>" +
        "<dt>fields</dt><dd>" + esc(secret.field_count != null ? secret.field_count : "") + "</dd>" +
        "<dt>sensitive fields</dt><dd>" + esc(secret.sensitive_field_count != null ? secret.sensitive_field_count : "") + "</dd>" +
        "<dt>vault-key binding</dt><dd>" + esc(binding) + "</dd>" +
        "</dl></div>" +
        '<div class="panel"><h2>fields</h2><div class="list">' +
        (fields || '<div class="empty">no fields</div>') + "</div></div>";
    }).catch(secretsError);
  }

  // --- receive-only email ---------------------------------------------
  // The browser receives a purpose-built projection with no email/message
  // ids, decoded body, MIME/header material, attachment detail, or processing
  // fence. There are deliberately no per-message actions in this view.
  function emailQuery() {
    var params = ["limit=100"];
    if (state.emailFilters.unread) { params.push("unread=true"); }
    if (state.emailFilters.unacked) { params.push("unacked=true"); }
    return params.join("&");
  }

  function emailUnavailableReason(err) {
    var message = String((err && err.message) || err || "").toLowerCase();
    if ((err && err.status === 403) || message.indexOf("not enrolled") >= 0) { return "not_enrolled"; }
    return "unavailable";
  }

  function renderEmailUnavailable(reason) {
    var message = reason === "not_enrolled"
      ? "this agent is not enrolled in receive-only email."
      : "receive-only email is not available on this cell right now.";
    $("view").innerHTML = '<div class="panel"><h2>email <span class="badge">metadata only</span></h2>' +
      '<div class="empty">' + esc(message) + "</div></div>";
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
    if (!value) { return ""; }
    if (value < 1024) { return value + " B"; }
    if (value < 1024 * 1024) { return (value / 1024).toFixed(1) + " KiB"; }
    return (value / (1024 * 1024)).toFixed(1) + " MiB";
  }

  function renderEmailList() {
    var address = state.emailAddress || {};
    var rows = (state.emailMessages || []).map(function (message) {
      var read = (message.read_state || {}).state || "unknown";
      var processing = (message.processing || {}).state || "unknown";
      var senderState = message.sender_verification_state || "unverified";
      var flags = [];
      if (message.attachment_count) {
        flags.push(message.attachment_count + " attachment" + (message.attachment_count === 1 ? "" : "s") + " (details hidden)");
      }
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
    $("view").innerHTML = '<div class="panel"><h2>receive address <span class="badge">' +
      esc(address.receive_state || "unknown") + "</span></h2>" +
      '<div class="email-address mono">' + esc(address.address || "") + "</div>" +
      '<div class="email-note">agent receive: ' + esc(address.agent_receive_state || "unknown") +
      ' · realm receive: ' + esc(address.realm_receive_state || "unknown") + "</div>" +
      '<div class="email-note">receive-only; sender identity and all subjects are untrusted external input.</div></div>' +
      '<div class="panel"><h2>email <span class="badge">metadata only</span></h2>' +
      '<div class="email-controls"><label><input id="email-unread" type="checkbox"' +
      (state.emailFilters.unread ? " checked" : "") + '> unread only</label>' +
      '<label><input id="email-unacked" type="checkbox"' +
      (state.emailFilters.unacked ? " checked" : "") + '> unacknowledged only</label></div>' +
      '<div class="email-note">body text, raw MIME, attachment details, message identifiers, and processing claims never enter this page.</div>' +
      '<div class="list">' + (rows || '<div class="empty">no matching email</div>') + "</div></div>";
    bindEmailControls();
  }

  function openEmailEvents() {
    openEvents(null, 0, false, false, false, false, true,
      state.emailFilters.unread, state.emailFilters.unacked);
  }

  function refreshEmail() {
    return fetchJSON("/api/email?" + emailQuery()).then(function (body) {
      state.emailAvailable = body.available !== false;
      state.emailMessages = body.messages || [];
      renderEmailList();
      openEmailEvents();
    }).catch(function (err) {
      state.emailAvailable = false;
      state.emailMessages = [];
      openEvents(null);
      renderEmailUnavailable(emailUnavailableReason(err));
    });
  }

  function bindEmailControls() {
    [["email-unread", "unread"], ["email-unacked", "unacked"]].forEach(function (pair) {
      var input = $(pair[0]);
      if (!input) { return; }
      input.addEventListener("change", function () {
        state.emailFilters[pair[1]] = input.checked;
        refreshEmail();
      });
    });
  }

  function viewEmail() {
    breadcrumb([{ label: "email" }]);
    // Keep the self/checkpoint stream active while enrollment is resolved;
    // only start email polling after the read-only address probe succeeds.
    openEvents(null);
    fetchJSON("/api/email/address").then(function (body) {
      state.emailAddress = body.address || null;
      state.emailAvailable = body.available !== false;
      return refreshEmail();
    }).catch(function (err) {
      state.emailAddress = null;
      state.emailMessages = [];
      state.emailAvailable = false;
      renderEmailUnavailable(emailUnavailableReason(err));
    });
  }

  // --- conversations ----------------------------------------------------
  // Thread-grouped view of the realm-local mailbox built strictly from the
  // passive metadata-only list (/api/messages, upstream GET /v1/messages).
  // The dashboard never calls :read or :listen, and the passive list never
  // carries bodies, so bubbles render metadata only. The upstream cursor
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
    var rows = buildThreads().map(function (thread) {
      return '<div class="row"><span class="grow"><a href="#/conversations/' + esc(encodeURIComponent(thread.key)) + '">' + esc(thread.label) + "</a>" +
        (thread.unread ? ' <span class="unread">' + esc(thread.unread) + "</span>" : "") + "</span>" +
        '<span class="dim">' + esc(thread.messages.length) + " msg" + (thread.messages.length === 1 ? "" : "s") + "</span>" +
        '<span class="dim">' + esc(thread.latestAt.slice(0, 19)) + "</span></div>";
    }).join("");
    var caret = captureFilterFocus("conversations");
    $("view").innerHTML = '<div class="panel"><h2>conversations</h2>' + filterInputHTML("conversations") +
      '<div class="list">' + (rows || '<div class="empty">no messages</div>') + "</div></div>";
    bindFilter("conversations");
    restoreFilterFocus("conversations", caret);
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
      '<div class="nobody">[body not shown]</div>' +
      '<div class="meta mono">' + esc(meta.join(" \u00b7 ")) + "</div></div>";
  }

  function renderConversation(key) {
    var thread = null;
    buildThreads().forEach(function (candidate) { if (candidate.key === key) { thread = candidate; } });
    var bubbles = thread ? thread.messages.map(bubbleHTML).join("") : "";
    $("view").innerHTML = '<div class="panel"><h2>' + esc(thread ? thread.label : key) +
      ' <span class="badge">metadata only</span></h2>' +
      '<div class="thread-note">message bodies are not passively readable yet &mdash; the only body read today (:read) marks messages read. ' +
      "a server-side observational body read is a planned follow-up.</div>" +
      '<div class="bubbles">' + (bubbles || '<div class="empty">no messages in this thread</div>') + "</div></div>";
    var view = $("view");
    view.scrollTop = view.scrollHeight;
  }

  function viewConversations() {
    breadcrumb([{ label: "conversations" }]);
    openEvents(null, 0, true);
    fetchMessages().then(renderConversationList).catch(showError);
  }

  function viewConversation(key) {
    breadcrumb([{ label: "conversations", href: "#/conversations" }, { label: key }]);
    openEvents(null, 0, true);
    fetchMessages().then(function () { renderConversation(key); }).catch(showError);
  }

  // --- boot -------------------------------------------------------------
  initTheme();
  $("status-addr").textContent = window.location.host;
  $("view").addEventListener("click", onRevealClick);
  window.addEventListener("hashchange", route);
  route();
})();
