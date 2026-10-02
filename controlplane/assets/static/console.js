// zenchron-engineering control plane console: small plain JavaScript, no
// framework, no build step.
//
// This script does exactly three things, all read-only:
//   1. Bootstraps the session cookie from the login page, entirely in the
//      browser - the token never travels in a URL, a query string, or a
//      server log, only in the Cookie header this sets and the Authorization
//      header requests below attach from it.
//   2. Keeps the top bar's live/stale indicator honest by polling one of
//      API's own schema'd /v1/* routes and marking the console STALE the
//      moment a poll fails - an operator must never read a frozen page as
//      current.
//   3. On a run's detail page, patches the current-operation panel and tails
//      new journal events into the timeline from API's /v1/runs/{id} and
//      /v1/runs/{id}/events, so the live overlay updates without a reload
//      losing the operator's place.
//
// Every fetch here targets controlplane.API's existing, published /v1/*
// contract (controlplane/api.go) - never a route this console invents for
// itself - and nothing here ever issues a mutating request.
(function () {
  "use strict";

  var POLL_MS = 4000;
  var COOKIE_NAME = "zenchron_control_plane_token";

  function getCookie(name) {
    var match = document.cookie.match(new RegExp("(?:^|; )" + name + "=([^;]*)"));
    return match ? decodeURIComponent(match[1]) : null;
  }

  function setCookie(name, value) {
    document.cookie = name + "=" + encodeURIComponent(value) + "; path=/; SameSite=Strict";
  }

  function authHeaders() {
    var token = getCookie(COOKIE_NAME);
    return token ? { Authorization: "Bearer " + token } : {};
  }

  // ---------------------------------------------------------------------
  // Login
  // ---------------------------------------------------------------------
  var loginForm = document.getElementById("login-form");
  if (loginForm) {
    loginForm.addEventListener("submit", function (event) {
      event.preventDefault();
      var input = document.getElementById("token");
      var value = input && input.value ? input.value.trim() : "";
      if (!value) return;
      setCookie(COOKIE_NAME, value);
      // Confirm the token against a real, cheap, already-authenticated
      // route before reloading the page the operator was on: a wrong token
      // would otherwise bounce back to this same login page with no
      // explanation.
      fetch("/v1/controller", { headers: authHeaders() })
        .then(function (res) {
          if (res.ok) {
            window.location.reload();
            return;
          }
          var banner = document.getElementById("login-error");
          if (banner) banner.hidden = false;
        })
        .catch(function () {
          var banner = document.getElementById("login-error");
          if (banner) banner.hidden = false;
        });
    });
  }

  // ---------------------------------------------------------------------
  // Live / stale indicator
  // ---------------------------------------------------------------------
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
    fetch(url, { headers: Object.assign({ Accept: "application/json" }, authHeaders()) })
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

  function patchOperationPanel(panel, run) {
    var op = run && run.operation;
    if (!op) return;
    setStateField(panel, "state", op.state);
    setField(panel, "progress-source", op.progress_source || "journal");
    setField(panel, "last-progress-at", formatTimestamp(op.last_progress_at));
    setField(panel, "silent-for", formatDuration(op.silent_for));
  }

  function appendTimelineEntry(list, event) {
    if (list.querySelector('[data-sequence="' + event.sequence + '"]')) return;
    var item = document.createElement("li");
    item.setAttribute("data-sequence", event.sequence);

    var seq = document.createElement("span");
    seq.className = "mono muted";
    seq.textContent = "#" + event.sequence;
    var kind = document.createElement("span");
    kind.className = "event-type";
    kind.textContent = event.type;
    var at = document.createElement("span");
    at.className = "muted";
    at.textContent = formatTimestamp(event.occurred_at);

    item.appendChild(seq);
    item.appendChild(kind);
    item.appendChild(at);
    list.appendChild(item);
  }

  function startRunDetailPolling(runID) {
    var panel = document.getElementById("operation-panel");
    var list = document.getElementById("timeline");
    var after = list ? parseInt(list.getAttribute("data-after") || "0", 10) : 0;

    function tick() {
      pollJSON("/v1/runs/" + encodeURIComponent(runID), function (run) {
        if (panel) patchOperationPanel(panel, run);
      });
      if (list) {
        pollJSON("/v1/runs/" + encodeURIComponent(runID) + "/events?after=" + after, function (page) {
          var events = page.events || [];
          for (var i = 0; i < events.length; i++) {
            appendTimelineEntry(list, events[i]);
          }
          if (typeof page.next_after === "number") after = page.next_after;
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
      startHeartbeat("/v1/controller");
    } else if (path === "/runs") {
      startHeartbeat("/v1/runs");
    }

    // Keyboard-friendly: "/" focuses the source filter on the runs list,
    // "r" reloads the current page, matching the console's own dense,
    // operator-facing conventions rather than introducing a shortcut layer.
    document.addEventListener("keydown", function (event) {
      if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey) return;
      var target = event.target;
      var typing = target && (target.tagName === "INPUT" || target.tagName === "TEXTAREA");
      if (typing) return;
      if (event.key === "/") {
        var source = document.getElementById("source");
        if (source) {
          event.preventDefault();
          source.focus();
        }
      } else if (event.key === "r") {
        window.location.reload();
      }
    });
  });
})();
