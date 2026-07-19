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
    seenSequences: {},   // transcript id -> highest rendered sequence
    messages: {},        // direction + " " + message id -> passive metadata
    lastSelfData: null,     // last raw "self" frame, to skip no-op re-renders
    lastMemoriesData: null, // last raw "memories" frame, same reason
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
  // edit (ADR 0004). Unknown names fall back to the default.
  function applyTheme(name) {
    if (state.themes.indexOf(name) < 0) {
      name = state.themes.indexOf("console") >= 0 ? "console" : state.themes[0];
    }
    $("theme-css").setAttribute("href", "/static/themes/" + encodeURIComponent(name) + ".css");
    $("theme-select").value = name;
    try { localStorage.setItem(THEME_KEY, name); } catch (_) { /* private mode */ }
  }

  function initTheme() {
    var fromQuery = new URLSearchParams(window.location.search).get("theme");
    var stored = null;
    try { stored = localStorage.getItem(THEME_KEY); } catch (_) { /* private mode */ }
    fetchJSON("/api/themes").then(function (body) {
      var names = (body.themes || []).filter(function (name) {
        return /^[A-Za-z0-9][A-Za-z0-9_.-]*$/.test(name);
      });
      if (names.length) { state.themes = names; }
    }).catch(function () { /* keep the built-in default */ }).then(function () {
      $("theme-select").innerHTML = state.themes.map(function (name) {
        return '<option value="' + esc(name) + '">' + esc(name) + "</option>";
      }).join("");
      applyTheme(fromQuery || stored || "console");
    });
    $("theme-select").addEventListener("change", function (event) {
      applyTheme(event.target.value);
    });
  }

  // --- data -------------------------------------------------------------
  function fetchJSON(path) {
    return fetch(path, { credentials: "same-origin" }).then(function (resp) {
      if (!resp.ok) {
        return resp.json().catch(function () { return {}; }).then(function (body) {
          throw new Error(body.error || (path + ": HTTP " + resp.status));
        });
      }
      return resp.json();
    });
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

  function setSSEState(up) {
    var dot = $("live-dot");
    dot.classList.toggle("up", up === true);
    dot.classList.toggle("down", up === false);
    $("live-label").textContent = up === true ? "live" : (up === false ? "offline" : "connecting");
    $("status-sse").textContent = "sse " + (up === true ? "connected" : (up === false ? "reconnecting" : "idle"));
  }

  // --- server-sent events ----------------------------------------------
  function openEvents(transcriptID, afterSequence, withMessages, withMemories) {
    withMessages = withMessages === true;
    withMemories = withMemories === true;
    if (state.eventSource && state.sseTranscript === (transcriptID || null) &&
        state.sseMessages === withMessages && state.sseMemories === withMemories) { return; }
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
    var source = new EventSource("/api/events" + (params.length ? "?" + params.join("&") : ""));
    state.eventSource = source;
    state.sseTranscript = transcriptID || null;
    state.sseMessages = withMessages;
    state.sseMemories = withMemories;
    source.onopen = function () { setSSEState(true); };
    source.onerror = function () { setSSEState(false); };
    source.addEventListener("self", function (event) {
      setSSEState(true);
      // The digest carries no clock, so identical frames mean nothing
      // changed; skip the no-op re-render.
      if (event.data === state.lastSelfData) { return; }
      state.lastSelfData = event.data;
      var self;
      try { self = JSON.parse(event.data); } catch (_) { return; }
      renderHeader(self);
      if (parseHash().section === "overview") { renderOverview(self); }
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
    if (route.section === "memories" && route.id) { return viewMemory(route.id); }
    if (route.section === "memories") { return viewMemories(); }
    if (route.section === "conversations" && route.id) { return viewConversation(decodeURIComponent(route.id)); }
    if (route.section === "conversations") { return viewConversations(); }
    return viewOverview();
  }

  function showError(err) {
    $("view").innerHTML = '<div class="error">' + esc(err.message || err) + "</div>";
  }

  // --- views ------------------------------------------------------------
  function renderOverview(self) {
    var counts = (self.index && self.index.counts) || {};
    var cards = Object.keys(counts).sort().map(function (key) {
      return '<div class="card"><div class="num">' + esc(counts[key]) + '</div><div class="label">' + esc(key) + "</div></div>";
    }).join("");
    var salient = (self.salient_memories || []).map(function (memory) {
      return '<div class="row"><span class="grow"><a href="#/memories/' + esc(memory.id) + '">' + esc(memory.snippet || memory.id) + "</a></span>" +
        '<span class="dim">' + esc(memory.kind || "") + '</span><span class="dim mono">' + esc((memory.salience != null ? memory.salience.toFixed(2) : "")) + "</span></div>";
    }).join("");
    var checkpoints = [];
    if (self.memory_checkpoint && self.memory_checkpoint.pending) { checkpoints.push("memory curation pending"); }
    if (self.message_checkpoint && self.message_checkpoint.pending) { checkpoints.push("messaging work pending"); }
    if (self.avatar_checkpoint && self.avatar_checkpoint.pending) { checkpoints.push("avatar lifecycle pending"); }
    $("view").innerHTML =
      '<div class="panel"><h2>inventory</h2><div class="cards">' + (cards || '<span class="empty">no counts</span>') + "</div></div>" +
      '<div class="panel"><h2>salient memories</h2><div class="list">' + (salient || '<div class="empty">none</div>') + "</div></div>" +
      '<div class="panel"><h2>checkpoints</h2><div class="list">' +
      (checkpoints.length ? checkpoints.map(function (line) { return '<div class="row"><span class="grow">' + esc(line) + "</span></div>"; }).join("") : '<div class="empty">nothing pending</div>') +
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
      $("view").innerHTML = '<div class="panel"><h2>transcripts</h2><div class="list">' +
        (rows || '<div class="empty">no transcripts</div>') + "</div></div>";
    }).catch(showError);
  }

  function entryHTML(entry, anchored) {
    var role = (entry.role || "").toLowerCase().replace(/[^a-z]/g, "") || "unknown";
    return '<div class="entry role-' + esc(role) + (anchored ? " anchored" : "") + '" data-seq="' + esc(entry.sequence) + '">' +
      '<span class="seq">' + esc(entry.sequence) + "</span>" +
      '<span class="role">' + esc(entry.role || "?") + "</span>" +
      '<span class="body">' + esc(entry.body || (entry.payload ? "[payload]" : "")) + "</span></div>";
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

  function renderMemoriesList(page) {
    var rows = (page.items || []).map(function (memory) {
      var label = memory.redacted ? "[redacted " + (memory.kind || "memory") + "]" : (memory.content || memory.id);
      return '<div class="row"><span class="grow"><a href="#/memories/' + esc(memory.id) + '">' + esc(label) + "</a></span>" +
        '<span class="dim">' + esc(memory.kind || "") + "</span>" +
        '<span class="dim">' + esc(memory.state || "") + "</span>" +
        '<span class="dim mono">' + esc(memory.salience != null ? memory.salience.toFixed(2) : "") + "</span></div>";
    }).join("");
    $("view").innerHTML = '<div class="panel"><h2>memories</h2><div class="list">' +
      (rows || '<div class="empty">no memories</div>') + "</div></div>";
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
    $("view").innerHTML = '<div class="panel"><h2>conversations</h2><div class="list">' +
      (rows || '<div class="empty">no messages</div>') + "</div></div>";
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
  window.addEventListener("hashchange", route);
  route();
})();
