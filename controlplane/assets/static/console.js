// zenchron-engineering control plane console: small plain JavaScript, no
// framework, no build step.
//
// This script does exactly three things:
//   1. Bootstraps the session cookie from the login page, entirely in the
//      browser - the token never travels in a URL, a query string, or a
//      server log, only in the Cookie header this sets and the Authorization
//      header requests below attach from it.
//   2. Keeps every page current by re-fetching its own server render and
//      swapping the [data-live] regions, and keeps the top bar's live/stale
//      indicator honest: "live" only while that refresh is succeeding,
//      "stale" the moment it fails, times out or goes quiet - an operator
//      must never read a frozen page as current (#420).
//   3. Governed operator actions (#398): a [data-action] button opens a
//      confirm dialog naming the exact target and controller generation;
//      only the dialog's confirm sends ONE POST, with the Bearer header, and
//      shows the outcome and resulting durable state. It never retries.
//
// Every fetch here targets a route the server already serves - the page
// itself, API's published /v1/controller for the login check, or one of the
// two fixed action routes a rendered button names - never a route this
// console invents.
(function () {
  "use strict";

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
  // Live refresh and the live / stale indicator
  // ---------------------------------------------------------------------
  // Each tick re-fetches this page's own server render - the same
  // authenticated GET, the same sanitized projections - and swaps every
  // [data-live] region by id. One request covers every region, so one panel
  // can never read fresh while another failed (#420). "live" means the last
  // full refresh succeeded within STALE_MS; anything else is "stale" with
  // the age of the data actually on screen.
  // ponytail: whole-page re-render per tick; switch run detail to #396's
  // SSE stream and the fleet to #399's change cursor once they land.
  var POLL_MS = 2000;
  var TIMEOUT_MS = 3000;
  var STALE_MS = 5000;
  var indicator = document.getElementById("live-indicator");
  var freshness = document.getElementById("freshness");
  var lastGood = Date.parse(freshness && freshness.getAttribute("data-observed-at")) || Date.now();
  var failure = "";
  // confirmed is false until one refresh has succeeded in this page. Before
  // then the server snapshot is all there is, so the badge says "snapshot"
  // (or "stale" once that snapshot is too old) and never "live" (#421 review).
  var confirmed = false;

  function render() {
    if (!indicator) return;
    var age = Date.now() - lastGood;
    var live = confirmed && !failure && age <= STALE_MS;
    if (!confirmed && !failure && age <= STALE_MS) return;
    indicator.textContent = live ? "live" : "stale";
    indicator.classList.toggle("stale", !live);
    indicator.title = live ? "refreshed within the last " + STALE_MS / 1000 + "s" : failure || "no successful refresh for " + Math.round(age / 1000) + "s";
    if (freshness) freshness.textContent = "updated " + new Date(lastGood).toISOString().slice(11, 19) + "Z" + (live ? "" : " (" + Math.round(age / 1000) + "s ago)");
  }

  function swapRegions(doc) {
    var regions = document.querySelectorAll("[data-live][id]");
    for (var i = 0; i < regions.length; i++) {
      if (!doc.getElementById(regions[i].id)) throw new Error("region " + regions[i].id + " missing from refresh");
    }
    var focused = document.activeElement;
    for (var j = 0; j < regions.length; j++) {
      var next = document.importNode(doc.getElementById(regions[j].id), true);
      var href = regions[j].contains(focused) && focused.getAttribute("href");
      regions[j].replaceWith(next);
      if (href) {
        var again = next.querySelector('[href="' + CSS.escape(href) + '"]');
        if (again) again.focus();
      }
    }
  }

  function refresh() {
    var ctrl = new AbortController();
    var timer = window.setTimeout(function () { ctrl.abort(); }, TIMEOUT_MS);
    fetch(window.location.href, { headers: { Accept: "text/html" }, credentials: "same-origin", signal: ctrl.signal })
      .then(function (res) {
        if (!res.ok) throw new Error("refresh failed: http " + res.status);
        return res.text();
      })
      .then(function (html) {
        swapRegions(new DOMParser().parseFromString(html, "text/html"));
        lastGood = Date.now();
        failure = "";
        confirmed = true;
      })
      .catch(function (err) {
        failure = err && err.name === "AbortError" ? "refresh timed out after " + TIMEOUT_MS / 1000 + "s" : String(err && err.message || err);
      })
      .then(function () {
        window.clearTimeout(timer);
        render();
        // Chained, not setInterval: a slow render never stacks requests.
        window.setTimeout(refresh, POLL_MS);
      });
  }

  // ---------------------------------------------------------------------
  // Governed actions: confirm, send once, show the durable result
  // ---------------------------------------------------------------------
  var dialog = document.getElementById("action-dialog");
  var pending = null;
  var inflight = false;

  function byId(id) { return document.getElementById(id); }

  function describe(state) {
    if (!state) return "";
    if (state.disposition) return "; run is now " + state.disposition;
    if (state.shown) return "; revision " + state.revision + " is now " + state.shown;
    return "";
  }

  function settle(outcome, text) {
    var result = byId("action-result");
    result.setAttribute("data-outcome", outcome);
    result.textContent = text;
    inflight = false;
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest && event.target.closest("[data-action]");
    if (!button || !dialog || inflight) return;
    // Copied now: the live refresh replaces the button underneath the dialog.
    pending = Object.assign({}, button.dataset);
    byId("action-target").textContent = pending.target;
    byId("action-generation").textContent = pending.generation;
    byId("action-consequence").textContent = pending.consequence;
    byId("action-note-field").hidden = !pending.revision;
    byId("action-note").value = "";
    byId("action-result").textContent = "";
    byId("action-result").removeAttribute("data-outcome");
    byId("action-confirm").disabled = false;
    dialog.showModal();
  });

  if (dialog) {
    byId("action-close").addEventListener("click", function () { if (!inflight) dialog.close(); });
    dialog.addEventListener("cancel", function (event) { if (inflight) event.preventDefault(); });
    byId("action-confirm").addEventListener("click", function () {
      if (!pending || inflight) return;
      byId("action-confirm").disabled = true; // one dialog, one send
      settle("pending", "pending: sent once, waiting for the controller...");
      inflight = true;
      var body = { controller_binding: pending.binding };
      if (pending.revision) {
        body.revision = Number(pending.revision);
        body.digest = pending.digest;
        if (byId("action-note").value) body.note = byId("action-note").value;
      }
      var headers = authHeaders();
      headers["Content-Type"] = "application/json";
      fetch(pending.action, { method: "POST", headers: headers, body: JSON.stringify(body) })
        .then(function (res) { return res.json(); })
        .then(function (r) {
          if (r.error) return settle("refused", "refused before sending: " + r.error);
          settle(r.outcome, r.outcome + (r.code ? " (" + r.code + ")" : "") + (r.detail ? ": " + r.detail : "") + describe(r.state));
        })
        .catch(function () {
          settle("unknown", "outcome not confirmed: no answer from the control plane; re-read the page before retrying");
        });
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    if (document.querySelector("[data-live][id]")) {
      refresh();
      window.setInterval(render, 1000);
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
