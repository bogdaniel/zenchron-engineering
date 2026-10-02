// zenchron-engineering control plane: small plain JavaScript, no framework.
//
// This script does exactly two things, both read-only:
//   1. Keeps the top bar's live/stale indicator honest, by polling this
//      page's own /api/* counterpart and marking the console STALE the
//      moment a poll fails - an operator must never read a frozen page as
//      current.
//   2. On a run's detail page, patches the current-operation panel and tails
//      new journal events into the timeline, so the live overlay updates
//      without a full reload losing the operator's place (expanded
//      <details>, scroll position, filter state).
//
// Every fetch targets this console's own /api/* routes (see
// control_plane_server.go's own comment: these are the page's polling
// mechanism, not a published contract) and nothing here ever issues a
// mutating request.
(function () {
  "use strict";

  var POLL_MS = 4000;
  var indicator = document.getElementById("live-indicator");

  function markLive() {
    if (!indicator) return;
    indicator.textContent = "live";
    indicator.classList.remove("stale");
    indicator.removeAttribute("title");
  }

  function markStale(detail) {
    if (!indicator) return;
    indicator.textContent = "stale";
    indicator.classList.add("stale");
    if (detail) indicator.title = detail;
  }

  function pollJSON(url, onData) {
    fetch(url, { headers: { Accept: "application/json" }, credentials: "same-origin" })
      .then(function (res) {
        if (!res.ok) throw new Error("http " + res.status);
        return res.json();
      })
      .then(function (data) {
        markLive();
        onData(data);
      })
      .catch(function (err) {
        markStale(err && err.message ? err.message : String(err));
      });
  }

  function setField(root, field, value) {
    var el = root.querySelector('[data-field="' + field + '"]');
    if (el) el.textContent = value;
  }

  function setStateField(root, field, value) {
    var el = root.querySelector('[data-field="' + field + '"]');
    if (el) {
      el.textContent = value;
      el.setAttribute("data-state", value);
    }
  }

  // formatDuration mirrors time.Duration.Round(time.Second).String() closely
  // enough for a live-patched value: whole seconds, minutes and hours, no
  // sub-second noise.
  function formatDuration(nanos) {
    if (!nanos || nanos <= 0) return "0s";
    var totalSeconds = Math.round(nanos / 1e9);
    var hours = Math.floor(totalSeconds / 3600);
    var minutes = Math.floor((totalSeconds % 3600) / 60);
    var seconds = totalSeconds % 60;
    var out = "";
    if (hours) out += hours + "h";
    if (hours || minutes) out += minutes + "m";
    out += seconds + "s";
    return out;
  }

  function formatTimestamp(iso) {
    if (!iso) return "—";
    var d = new Date(iso);
    if (isNaN(d.getTime())) return iso;
    return d.toISOString().replace("T", " ").replace(/\.\d+Z$/, " UTC");
  }

  function patchOperationPanel(panel, detail) {
    var op = detail && detail.operation;
    if (!op) return;
    setStateField(panel, "state", op.state);
    setField(panel, "progress-source", op.progress_source);
    setField(panel, "last-progress-at", formatTimestamp(op.last_progress_at));
    setField(panel, "silent-for", formatDuration(op.silent_for));
  }

  function appendTimelineEntry(list, event) {
    if (list.querySelector('[data-sequence="' + event.sequence + '"]')) {
      return;
    }
    var item = document.createElement("li");
    item.setAttribute("data-sequence", event.sequence);
    var details = document.createElement("details");
    details.open = true;
    var summary = document.createElement("summary");

    var seq = document.createElement("span");
    seq.className = "mono muted";
    seq.textContent = "#" + event.sequence;
    var kind = document.createElement("span");
    kind.className = "event-type";
    kind.textContent = event.type;
    var at = document.createElement("span");
    at.className = "muted";
    at.textContent = formatTimestamp(event.occurred_at);
    var actor = document.createElement("span");
    actor.className = "muted";
    actor.textContent = "by " + event.actor;

    summary.appendChild(seq);
    summary.appendChild(kind);
    summary.appendChild(at);
    summary.appendChild(actor);
    details.appendChild(summary);

    var body = document.createElement("div");
    body.className = "event-body";
    body.innerHTML = "";
    var note = document.createElement("p");
    note.className = "muted";
    note.textContent = "new event: reload for the full causal detail.";
    body.appendChild(note);
    details.appendChild(body);

    item.appendChild(details);
    list.insertBefore(item, list.firstChild);
  }

  function highestSequence(list) {
    var max = 0;
    list.querySelectorAll("[data-sequence]").forEach(function (li) {
      var seq = parseInt(li.getAttribute("data-sequence"), 10);
      if (seq > max) max = seq;
    });
    return max;
  }

  function startRunDetailPolling(runID) {
    var panel = document.getElementById("operation-panel");
    var list = document.getElementById("timeline");
    var after = list ? highestSequence(list) : 0;

    function tick() {
      pollJSON("/api/runs/" + encodeURIComponent(runID), function (data) {
        // runDetailData (control_plane_server.go) carries no json tags of its
        // own, so its wrapper fields serialize under their Go names; only the
        // runtime.RunDetail it wraps uses the lowercase tags read below.
        if (panel) patchOperationPanel(panel, data.Detail);
      });
      if (list) {
        pollJSON("/api/runs/" + encodeURIComponent(runID) + "/events?after=" + after, function (page) {
          var events = page.events || [];
          for (var i = events.length - 1; i >= 0; i--) {
            appendTimelineEntry(list, events[i]);
          }
          if (typeof page.next_after === "number") {
            after = page.next_after;
          }
        });
      }
    }

    tick();
    return window.setInterval(tick, POLL_MS);
  }

  function startHeartbeat(url) {
    function tick() {
      pollJSON(url, function () {});
    }
    tick();
    return window.setInterval(tick, POLL_MS);
  }

  document.addEventListener("DOMContentLoaded", function () {
    var path = window.location.pathname;
    var runPanel = document.getElementById("operation-panel");
    if (runPanel && runPanel.dataset.runId) {
      startRunDetailPolling(runPanel.dataset.runId);
    } else if (path === "/overview" || path === "/") {
      startHeartbeat("/api/overview");
    } else if (path === "/runs") {
      startHeartbeat("/api/runs" + window.location.search);
    }
  });
})();
