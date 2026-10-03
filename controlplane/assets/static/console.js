// zenchron-engineering control plane console: small plain JavaScript, no
// framework, no build step.
//
// This script does exactly two things, all read-only:
//   1. Bootstraps the session cookie from the login page, entirely in the
//      browser - the token never travels in a URL, a query string, or a
//      server log, only in the Cookie header this sets and the Authorization
//      header requests below attach from it.
//   2. Keeps every page current by re-fetching its own server render and
//      swapping the [data-live] regions, and keeps the top bar's live/stale
//      indicator honest: "live" only while that refresh is succeeding,
//      "stale" the moment it fails, times out or goes quiet - an operator
//      must never read a frozen page as current (#420).
//
// Every fetch here targets a route the server already serves - the page
// itself, or API's published /v1/controller for the login check - never a
// route this console invents, and nothing here ever issues a mutating
// request.
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

  function render() {
    if (!indicator) return;
    var age = Date.now() - lastGood;
    var live = !failure && age <= STALE_MS;
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
