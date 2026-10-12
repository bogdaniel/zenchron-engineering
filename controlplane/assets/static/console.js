// Browser presentation over the existing governed Control Plane.
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
  var POLL_MS = 5000;
  var TIMEOUT_MS = 3000;
  var STALE_MS = 15000;
  var indicator = document.getElementById("live-indicator");
  var freshness = document.getElementById("freshness");
  var lastGood = Date.parse(freshness && freshness.getAttribute("data-observed-at")) || Date.now();
  var failure = "";
  var controllerUnavailable = false;
  // confirmed is false until one refresh has succeeded in this page. Before
  // then the server snapshot is all there is, so the badge says "snapshot"
  // (or "stale" once that snapshot is too old) and never "live" (#421 review).
  var confirmed = false;

  function render() {
    if (!indicator) return;
    var age = Date.now() - lastGood;
    var live = confirmed && !failure && age <= STALE_MS;
    if (!confirmed && !failure && age <= STALE_MS) return;
    indicator.textContent = live ? (controllerUnavailable ? "controller offline" : "live") : "stale";
    indicator.classList.toggle("stale", !live || controllerUnavailable);
    indicator.title = controllerUnavailable ? "Snapshots are current, but the execution controller is unavailable. Actions cannot proceed." : live ? "refreshed within the last " + STALE_MS / 1000 + "s" : failure || "no successful refresh for " + Math.round(age / 1000) + "s";
    if (freshness) freshness.textContent = "updated " + new Date(lastGood).toISOString().slice(11, 19) + "Z" + (live ? "" : " (" + Math.round(age / 1000) + "s ago)");
  }

  // shortcut: layout scans the validated 32-unit graph bound; revisit if that bound grows.
  function renderWorkGraphMaps() {
    document.querySelectorAll(".graph-map").forEach(function (map) {
      var nodes = Array.from(map.querySelectorAll("[data-unit-id]"));
      if (!nodes.length) return;
      var byId = new Map(nodes.map(function (node) { return [node.dataset.unitId, node]; }));
      var depthById = new Map();
      var visiting = new Set();
      function depth(id) {
        if (depthById.has(id)) return depthById.get(id);
        if (visiting.has(id)) return 0;
        visiting.add(id);
        var value = 0;
        (byId.get(id).dataset.dependsOn || "").trim().split(/\s+/).filter(Boolean).forEach(function (dependency) {
          if (byId.has(dependency)) value = Math.max(value, depth(dependency) + 1);
        });
        visiting.delete(id);
        depthById.set(id, value);
        return value;
      }
      var columns = [];
      nodes.forEach(function (node) {
        var level = depth(node.dataset.unitId);
        if (!columns[level]) columns[level] = [];
        columns[level].push(node);
      });
      map.style.setProperty("--graph-columns", String(columns.length));
      columns.forEach(function (column, index) {
        column.forEach(function (node, row) {
          node.style.gridColumn = String(index + 1);
          node.style.gridRow = String(row + 1);
        });
      });

      var svg = map.querySelector(".graph-edges");
      if (!svg) return;
      var defs = document.createElementNS(svg.namespaceURI, "defs");
      var marker = document.createElementNS(svg.namespaceURI, "marker");
      marker.setAttribute("id", "graph-arrow");
      marker.setAttribute("viewBox", "0 0 10 10");
      marker.setAttribute("refX", "9"); marker.setAttribute("refY", "5");
      marker.setAttribute("markerWidth", "6"); marker.setAttribute("markerHeight", "6"); marker.setAttribute("orient", "auto-start-reverse");
      var arrow = document.createElementNS(svg.namespaceURI, "path");
      arrow.setAttribute("d", "M 0 0 L 10 5 L 0 10 z"); arrow.setAttribute("fill", "var(--accent)");
      marker.appendChild(arrow); defs.appendChild(marker); svg.replaceChildren(defs);
      var bounds = map.getBoundingClientRect();
      svg.setAttribute("viewBox", "0 0 " + bounds.width + " " + bounds.height);
      nodes.forEach(function (node) {
        (node.dataset.dependsOn || "").trim().split(/\s+/).filter(Boolean).forEach(function (dependency) {
          var source = byId.get(dependency);
          if (!source) return;
          var from = source.getBoundingClientRect();
          var to = node.getBoundingClientRect();
          var x1 = from.right - bounds.left, y1 = from.top + from.height / 2 - bounds.top;
          var x2 = to.left - bounds.left, y2 = to.top + to.height / 2 - bounds.top;
          var bend = Math.max(28, (x2 - x1) / 2);
          var path = document.createElementNS(svg.namespaceURI, "path");
          path.classList.add("graph-edge");
          path.setAttribute("d", "M" + x1 + " " + y1 + " C" + (x1 + bend) + " " + y1 + "," + (x2 - bend) + " " + y2 + "," + x2 + " " + y2);
          path.setAttribute("marker-end", "url(#" + marker.id + ")");
          svg.appendChild(path);
        });
      });
    });
  }

  function reconcile(current, next) {
    if (current.isEqualNode(next)) return;
    if (current.nodeType !== next.nodeType || current.nodeName !== next.nodeName) {
      current.replaceWith(document.importNode(next, true));
      return;
    }
    if (current.nodeType === Node.TEXT_NODE) {
      current.nodeValue = next.nodeValue;
      return;
    }
    if (current.nodeType !== Node.ELEMENT_NODE) return;
    if (current.matches("input, textarea, select, form, #plan-approval, #plan-preview-status, #publication-actions")) return;
    Array.from(current.attributes).forEach(function (attribute) {
      if (!next.hasAttribute(attribute.name) && !(current.tagName === "DETAILS" && attribute.name === "open")) {
        current.removeAttribute(attribute.name);
      }
    });
    Array.from(next.attributes).forEach(function (attribute) {
      if (current.getAttribute(attribute.name) !== attribute.value) current.setAttribute(attribute.name, attribute.value);
    });
    var children = Array.from(current.childNodes);
    Array.from(next.childNodes).forEach(function (child, index) {
      if (!children[index]) current.appendChild(document.importNode(child, true));
      else reconcile(children[index], child);
    });
    children.slice(next.childNodes.length).forEach(function (child) { child.remove(); });
  }

  function swapRegions(doc) {
    var regions = document.querySelectorAll("[data-live][id]");
    for (var i = 0; i < regions.length; i++) {
      if (!doc.getElementById(regions[i].id)) throw new Error("region " + regions[i].id + " missing from refresh");
    }
    // A dialog shows exact bindings copied at open. Refresh after closing it.
    if (dialog && dialog.open) throw new Error("Updates paused while reviewing an operator action");
    var focused = document.activeElement;
    for (var j = 0; j < regions.length; j++) {
      var incoming = doc.getElementById(regions[j].id);
      if (regions[j].contains(focused) && !regions[j].isEqualNode(incoming)) {
        throw new Error("Updates paused to preserve your focused control; move focus outside the panel to refresh");
      }
    }
    for (var k = 0; k < regions.length; k++) reconcile(regions[k], doc.getElementById(regions[k].id));
  }

  function refresh() {
    if (document.hidden) { render(); window.setTimeout(refresh, POLL_MS); return; }
    var ctrl = new AbortController();
    var timer = window.setTimeout(function () { ctrl.abort(); }, TIMEOUT_MS);
    Promise.all([
      fetch(window.location.href, { headers: { Accept: "text/html" }, credentials: "same-origin", signal: ctrl.signal }),
      fetch("/v1/controller", { headers: authHeaders(), signal: ctrl.signal }).then(function (response) {
        if (!response.ok) throw new Error("Controller observation failed: HTTP " + response.status);
        return response.json();
      }).then(function (controller) { controllerUnavailable = !controller.live_reachable; })
    ]).then(function (responses) { return responses[0]; })
      .then(function (res) {
        if (!res.ok) throw new Error("refresh failed: http " + res.status);
        return res.text();
      })
      .then(function (html) {
        swapRegions(new DOMParser().parseFromString(html, "text/html"));
        renderWorkGraphMaps();
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
    byId("action-note-field").hidden = /\/(stop|resume|refresh)$/.test(pending.action);
    byId("action-value-field").hidden = pending.outcomeKind !== "text";
    byId("action-value").value = "";
    byId("action-note").value = "";
    byId("action-result").textContent = "";
    byId("action-result").removeAttribute("data-outcome");
    byId("action-confirm").disabled = false;
    dialog.showModal();
  });

  if (dialog) {
    byId("action-close").addEventListener("click", function () { if (!inflight) dialog.close(); });
    dialog.addEventListener("close", function () { loadPlanPreview(); loadPublicationRequest(); });
    dialog.addEventListener("cancel", function (event) { if (inflight) event.preventDefault(); });
    byId("action-confirm").addEventListener("click", function () {
      if (!pending || inflight) return;
      if (pending.outcomeKind === "text" && !byId("action-value").value.trim()) {
        byId("action-result").textContent = "Enter a decision response before confirming."; return;
      }
      byId("action-confirm").disabled = true; // one dialog, one send
      settle("pending", "pending: sent once, waiting for the controller...");
      inflight = true;
      var body = { controller_binding: pending.binding };
      if (pending.revision) {
        body.revision = Number(pending.revision);
        body.digest = pending.digest;
        if (pending.assignmentsDigest) body.assignments_digest = pending.assignmentsDigest;
      }
      if (pending.requestId) {
        body.request_id = pending.requestId;
        body.digest = pending.digest;
        body.decision = pending.decision;
      }
      if (pending.outcomeKind) {
        body.outcome_kind = pending.outcomeKind;
        body.outcome_value = pending.outcomeKind === "text" ? byId("action-value").value : pending.outcomeValue;
      }
      if (!/\/(stop|resume|refresh)$/.test(pending.action) && byId("action-note").value) body.note = byId("action-note").value;
      var headers = authHeaders();
      headers["Content-Type"] = "application/json";
      fetch(pending.action, { method: "POST", headers: headers, body: JSON.stringify(body), signal: AbortSignal.timeout(pending.action.endsWith("/revise") ? 1270000 : 30000) })
        .then(function (res) { return res.json().then(function (r) { r.httpStatus = res.status; return r; }); })
        .then(function (r) {
          // A bare error body is a boundary refusal (4xx, nothing sent) or a
          // server failure (5xx) after which nothing about the outcome is known.
          if (r.error && r.httpStatus >= 500) return settle("unknown", "outcome not confirmed (" + r.error + "): re-read before retrying");
          if (r.error) return settle("refused", "refused before sending: " + r.error);
          settle(r.outcome, r.outcome + (r.code ? " (" + r.code + ")" : "") + (r.detail ? ": " + r.detail : "") + describe(r.state));
        })
        .catch(function () {
          settle("unknown", "outcome not confirmed: no answer from the control plane; re-read the page before retrying");
        });
    });
  }

  function loadPlanPreview() {
    var container = byId("plan-approval");
    if (!container) return;
    container.replaceChildren();
    fetch("/v1/plans/" + encodeURIComponent(container.dataset.plan) + "/preview", { headers: authHeaders(), signal: AbortSignal.timeout(10000) })
      .then(function (response) {
        if (!response.ok) throw new Error("Exact assignment preview unavailable (HTTP " + response.status + ")");
        return response.json();
      }).then(function (plan) {
        var status = byId("plan-preview-status");
        status.textContent = "Revision " + plan.revision + " · " + (plan.shown || "unknown state");
        var objective = document.createElement("p"); objective.textContent = plan.objective || "Objective unavailable"; container.appendChild(objective);
        var stages = document.createElement("div"); stages.className = "graph-map";
        (plan.stages || []).forEach(function (stage) {
          var card = document.createElement("article"); card.className = "stat";
          var title = document.createElement("strong"); title.textContent = (plan.stage_objectives || {})[stage.stage_id] || stage.stage_id; card.appendChild(title);
          var worker = document.createElement("span"); worker.textContent = stage.kind === "agent" ? "Worker: " + (stage.agent || "Not bound") + " · " + (stage.provider_kind || "Unknown provider") + " · profile " + (stage.bound_profile || stage.profile || "any eligible") : stage.kind; card.appendChild(worker);
          var dependency = document.createElement("small"); dependency.textContent = "Depends on: " + ((stage.depends_on || []).join(", ") || "No dependencies"); card.appendChild(dependency);
          var rationale = document.createElement("p"); rationale.className = "muted"; rationale.textContent = (plan.stage_rationales || {})[stage.stage_id] || ""; card.appendChild(rationale);
          var blockers = (plan.stage_blockers || {})[stage.stage_id];
          if (blockers && blockers.length) {
            var blocked = document.createElement("p"); blocked.className = "banner";
            blocked.textContent = Array.isArray(blockers) ? blockers.join(" · ") : blockers;
            card.appendChild(blocked);
          }
          stages.appendChild(card);
        });
        container.appendChild(stages);
        var technical = document.createElement("details");
        var summary = document.createElement("summary"); summary.textContent = "Exact assignment and approval binding"; technical.appendChild(summary);
        var text = document.createElement("pre"); text.textContent = JSON.stringify(plan, null, 2); technical.appendChild(text); container.appendChild(technical);
        if ((plan.shown !== "proposed" && plan.shown !== "unapproved") || (plan.unbound && plan.unbound.length) || !plan.assignments_digest || !container.dataset.binding) {
          status.textContent += " · Approval unavailable: current proposed revision, valid assignment binding, and live controller are required.";
          return;
        }
        var button = document.createElement("button"); button.type = "button"; button.className = "action";
        button.textContent = "Approve exact plan…";
        Object.assign(button.dataset, { action: "/v1/plans/" + encodeURIComponent(container.dataset.plan) + "/approve", binding: plan.controller_binding || container.dataset.binding,
          revision: String(plan.revision), digest: plan.digest, assignmentsDigest: plan.assignments_digest,
          target: "Plan " + container.dataset.plan + " revision " + plan.revision + " digest " + plan.digest + " assignments " + plan.assignments_digest,
          generation: plan.controller_binding || container.dataset.binding, consequence: "Approval authorizes execution of this exact proposed revision and these exact assignments within the existing budget envelope. Policy and independent assurance remain required." });
        container.appendChild(button);
        var revise = document.createElement("button"); revise.type = "button"; revise.textContent = "Request revised proposal…";
        Object.assign(revise.dataset, { action: "/v1/plans/" + encodeURIComponent(container.dataset.plan) + "/revise",
          binding: plan.controller_binding || container.dataset.binding, revision: String(plan.revision), digest: plan.digest,
          target: "Plan " + container.dataset.plan + " revision " + plan.revision + " digest " + plan.digest,
          generation: plan.controller_binding || container.dataset.binding,
          consequence: "Requests a new AI proposal from the same objective. It does not approve that proposal or widen authority; existing governing work remains governed by its approved revision." });
        container.appendChild(revise);
      }).catch(function (error) { byId("plan-preview-status").textContent = error.message; });
  }

  function loadPublicationRequest() {
    var container = byId("publication-actions");
    if (!container) return;
    container.replaceChildren();
    var status = document.createElement("p"); status.setAttribute("role", "status");
    status.textContent = "Loading publication authority request…"; container.appendChild(status);
    fetch("/v1/runs/" + encodeURIComponent(container.dataset.run) + "/authority", { headers: authHeaders(), signal: AbortSignal.timeout(10000) })
      .then(function (response) {
        if (!response.ok) throw new Error("Publication unavailable (HTTP " + response.status + ")");
        return response.json();
      }).then(function (request) {
        if (!request || !request.id || !request.requires || !request.requires.length || !container.dataset.binding) {
          status.textContent = "No human publication authorization is currently available for this candidate.";
          return;
        }
        status.textContent = "Publication requires your decision for the exact candidate below.";
        var details = document.createElement("details");
        var summary = document.createElement("summary"); summary.textContent = "Candidate, contract, action, and evidence binding"; details.appendChild(summary);
        var exact = document.createElement("pre"); exact.textContent = JSON.stringify(request, null, 2); details.appendChild(exact); container.appendChild(details);
        ["approve", "reject"].forEach(function (decision) {
          var button = document.createElement("button"); button.type = "button"; button.className = "action";
          button.textContent = decision === "approve" ? "Authorize publication…" : "Reject publication…";
          Object.assign(button.dataset, { action: "/v1/runs/" + encodeURIComponent(container.dataset.run) + "/authorize", binding: container.dataset.binding,
            requestId: request.id, digest: request.digest, decision: decision,
            target: JSON.stringify({ request: request.id, candidate: request.candidate, contract: request.contract, action: request.action }),
            generation: container.dataset.binding, consequence: decision === "approve" ? "Records human approval evidence for this exact publication request. The kernel still verifies policy, final candidate eligibility, and independent assurance before publishing." : "Records failed human approval evidence and blocks this publication request." });
          container.appendChild(button);
        });
      }).catch(function (error) { status.textContent = error.message; });
  }

  function loadWorkspaceOptions(attempt) {
    attempt = attempt || 0;
    var form = byId("objective-form");
    var agents = byId("agents-list");
    var profiles = byId("profiles-list");
    var templates = byId("templates-list");
    if (!form && !agents && !profiles && !templates) return;
    fetch("/v1/workspace/options", { headers: authHeaders(), signal: AbortSignal.timeout(10000) })
      .then(function (response) {
        if (!response.ok) {
          var error = new Error("Controller configuration unavailable (HTTP " + response.status + ")");
          error.status = response.status;
          throw error;
        }
        return response.json();
      }).then(function (options) {
        if (agents) {
          agents.replaceChildren();
          byId("agents-status").textContent = options.agents.length ? "Configured execution agents" : "No agents configured";
          options.agents.forEach(function (agent) {
            var card = document.createElement("article");
            card.className = "stat";
            var heading = document.createElement("strong"); heading.textContent = agent.id; card.appendChild(heading);
            var detail = document.createElement("span"); detail.textContent = agent.kind + " · " + agent.trust_mode; card.appendChild(detail);
            var eligibility = document.createElement("span"); eligibility.textContent = agent.eligible ? "Eligible for execution" : "Unavailable for execution"; card.appendChild(eligibility);
            agents.appendChild(card);
          });
        }
        if (profiles) {
          profiles.replaceChildren();
          byId("profiles-status").textContent = options.profiles.length
            ? options.profiles.length + " installed profile" + (options.profiles.length === 1 ? "" : "s")
            : "No profiles configured. Plans may remain unassigned until the operator installs an eligible profile.";
          options.profiles.forEach(function (profile) {
            var card = document.createElement("article"); card.className = "stat";
            var heading = document.createElement("strong"); heading.textContent = profile; card.appendChild(heading);
            profiles.appendChild(card);
          });
        }
        if (templates) {
          templates.replaceChildren();
          byId("templates-status").textContent = options.templates.length
            ? options.templates.length + " installed plan template" + (options.templates.length === 1 ? "" : "s")
            : "No templates configured. The controller default remains available for planning.";
          options.templates.forEach(function (template) {
            var card = document.createElement("article"); card.className = "stat";
            var heading = document.createElement("strong"); heading.textContent = template; card.appendChild(heading);
            templates.appendChild(card);
          });
        }
        if (!form) return;
        function populate(id, values, label) {
          var select = byId(id); select.replaceChildren();
          values.forEach(function (value) {
            var option = document.createElement("option");
            option.value = typeof value === "string" ? value : value.id;
            option.textContent = label ? label(value) : option.value;
            select.appendChild(option);
          });
        }
        populate("objective-repository", options.repositories);
        populate("objective-agent", options.agents.filter(function (agent) { return agent.eligible; }), function (agent) { return agent.id + " · " + agent.kind; });
        populate("objective-profile", [""].concat(options.profiles), function (profile) { return profile || "Resolve from plan and policy"; });
        populate("objective-template", [""].concat(options.templates), function (template) { return template || "Controller default"; });
        var budget = options.budget;
        byId("objective-budget").textContent = "Maximum " + budget.max_child_runs + " worker runs · " + budget.max_concurrency + " concurrent · " + budget.max_provider_invocations + " provider invocations" +
          (budget.max_wall_seconds ? " · " + Math.round(budget.max_wall_seconds / 60) + " active minutes" : "") +
          (budget.max_cost_micros == null ? " · Cost ceiling unknown" : " · Cost ceiling " + budget.max_cost_micros + " micros") + ". Profile assignments resolve in the proposed plan.";
        byId("objective-submit").disabled = !form.dataset.binding || !options.repositories.length || !byId("objective-agent").options.length;
        if (!form.dataset.binding) byId("objective-status").textContent = "This development controller is unattested. Objective generation and approvals require an adopted controller.";
      }).catch(function (error) {
        var status = byId("objective-status") || byId("agents-status");
        if (!status) return;
        status.replaceChildren(document.createTextNode(error.status === 503
          ? error.message + ". Check that zenchron serve is running, then retry."
          : error.message));
        ["profiles-status", "templates-status"].forEach(function (id) {
          var detailStatus = byId(id);
          if (detailStatus) detailStatus.textContent = error.message;
        });
        if (error.status !== 503) return;
        var retry = document.createElement("button");
        retry.type = "button";
        retry.id = "workspace-options-retry";
        retry.textContent = "Retry controller connection";
        var retrying = false;
        function retryConfiguration() {
          if (retrying || !retry.isConnected) return;
          retrying = true;
          retry.remove();
          status.textContent = "Retrying controller configuration…";
          loadWorkspaceOptions(attempt + 1);
        }
        retry.addEventListener("click", function () {
          retryConfiguration();
        });
        status.appendChild(document.createTextNode(" "));
        status.appendChild(retry);
        if (attempt < 3) {
          status.appendChild(document.createTextNode(" Retrying automatically."));
          window.setTimeout(retryConfiguration, 1000 * Math.pow(2, attempt));
        }
      });
    if (form && !form.dataset.intakeBound) {
      form.dataset.intakeBound = "true";
      form.addEventListener("submit", function (event) {
        event.preventDefault();
        var submit = byId("objective-submit");
        if (submit.disabled || !form.reportValidity()) return;
        submit.disabled = true;
        var data = new FormData(form);
        var body = { controller_binding: form.dataset.binding, repository: data.get("repository"), issue: Number(data.get("issue")), agent: data.get("agent"), profile: data.get("profile"), template: data.get("template"), deterministic: data.get("deterministic") === "on" };
        var headers = authHeaders(); headers["Content-Type"] = "application/json";
        byId("objective-status").textContent = "Generating once. No execution is authorized by this request.";
        fetch("/v1/objectives", { method: "POST", headers: headers, body: JSON.stringify(body), signal: AbortSignal.timeout(1270000) })
          .then(function (response) { return response.json(); })
          .then(function (result) {
            var status = byId("objective-status");
            status.textContent = result.error || [result.outcome, result.code, result.detail].filter(Boolean).join(" · ");
            if (result.state && result.state.plan_id) {
              var link = document.createElement("a"); link.href = "/plans/" + encodeURIComponent(result.state.plan_id); link.textContent = "Inspect generated plan →";
              status.appendChild(document.createElement("br")); status.appendChild(link);
            }
          }).catch(function () { byId("objective-status").textContent = "Outcome unconfirmed. Inspect Plans before submitting again; this request is never retried automatically."; });
      });
    }
  }

  document.addEventListener("DOMContentLoaded", function () {
    renderWorkGraphMaps();
    loadWorkspaceOptions();
    loadPlanPreview();
    loadPublicationRequest();
    document.querySelectorAll(".topbar nav a").forEach(function (link) {
      if (window.location.pathname.startsWith(link.getAttribute("href"))) link.setAttribute("aria-current", "page");
    });
    var theme = byId("theme-toggle");
    try {
      var storedTheme = localStorage.getItem("zenchron-theme");
      if (storedTheme) document.documentElement.dataset.theme = storedTheme;
    } catch (_) { /* Storage is optional in restricted browser contexts. */ }
    if (theme) theme.addEventListener("click", function () {
      var dark = document.documentElement.dataset.theme === "dark" ||
        (!document.documentElement.dataset.theme && window.matchMedia("(prefers-color-scheme: dark)").matches);
      var choice = dark ? "light" : "dark";
      document.documentElement.dataset.theme = choice;
      try { localStorage.setItem("zenchron-theme", choice); } catch (_) { /* Theme still applies for this page. */ }
    });
    window.addEventListener("resize", renderWorkGraphMaps);
    if (!loginForm) {
      refresh();
      window.setInterval(render, 1000);
    }

    // Keyboard-friendly: "/" focuses the source filter on the runs list,
    // "r" reloads the current page, matching the console's own dense,
    // operator-facing conventions rather than introducing a shortcut layer.
    document.addEventListener("keydown", function (event) {
      if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey) return;
      var target = event.target;
      var typing = target && (target.matches("input, textarea, select") || target.isContentEditable);
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
