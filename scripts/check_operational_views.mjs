// HTTP projection fixtures test presentation only. They never supply governed acceptance evidence.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { resolve } from "node:path";
import { accessibilityIssues } from "./browser_accessibility.mjs";

export async function checkOperationalViews(page, base, out, captureFullPage) {
  let mode = "ready";
  const writes = [];
  const watch = (req) => {
    if (req.method() !== "GET") writes.push(req.url());
  };
  const run = {
    run_id: "test-run",
    repository: "example/payments",
    phase: "assurance",
    disposition: "waiting",
    elapsed: 480e9,
    active_elapsed: 300e9,
    external_wait_elapsed: 180e9,
    operation: null,
    source: { repository: "example/payments", issue: 42, url: "https://example.test/issues/42" },
    worker: { agent: "worker-1", provider_kind: "codex_cli", trust_mode: "operator_trusted", workspace_bound: true },
    candidate: { branch: "isolate-credentials", revision: "a".repeat(40), tree: "b".repeat(40) },
    contract: { id: "contract-42", revision: "c".repeat(64) },
    assurance: { passed: false, failure_class: "verification_failure" },
    authority_request: { status: "blocked", requires: ["independent_assurance"], missing: ["independent_assurance"] },
    held_material: {
      kind: "candidate",
      revision: "a".repeat(40),
      blocked_by: "independent_assurance",
      next_step: "await_evidence",
    },
  };
  const summary = {
    graph_id: "test-graph",
    name: "Credential isolation",
    repository: "example/payments",
    agent_id: "worker-1",
    revision: 3,
  };
  const product = {
    product_id: "test-product",
    name: "Payments platform",
    revision: 2,
    repositories: ["example/payments"],
  };
  const units = Array.from({ length: 1200 }, (_, i) => ({
    unit_id: `unit-${i}`,
    purpose: i === 0 ? "Isolate credentials <script>" : `Independent verification ${i}`,
    role: i === 0 ? "implementer" : "reviewer",
    issue: 42 + i,
    state: i === 0 ? "completed" : i === 1 ? "awaiting_decision" : "blocked",
    depends_on: i ? [`unit-${i - 1}`] : [],
    requires_review: true,
    review_approved: i === 0,
    ...(i === 0
      ? {
          run,
          output: {
            handoff_id: "handoff-42",
            candidate_revision: "a".repeat(40),
            candidate_tree: "b".repeat(40),
            summary: "Producer reports isolation; independently reviewed.",
            unresolved: ["Publication remains restricted"],
          },
          review: { verdict: "APPROVE", candidate: "a".repeat(40), reviewer: "reviewer-2", findings: [] },
        }
      : {}),
    ...(i === 1
      ? {
          hold: { reference: "request-1", detail: "Choose the bounded deployment scope" },
          open_decisions: [
            {
              id: "request-1",
              purpose: "Confirm deployment scope",
              expected_outcome_kind: "allow_deny",
              body: "Select the reviewed environment; no escalation.",
              requested_at: "2026-10-13T14:00:00Z",
            },
          ],
        }
      : {}),
  }));
  page.on("request", watch);
  await page.route("**/v1/**", async (route) => {
    const req = route.request(),
      url = new URL(req.url());
    assert.equal(req.method(), "GET", "This suite has no action authority");
    if (mode === "permission") return route.fulfill({ status: 403, json: { error: "forbidden_origin" } });
    if (mode === "failed") return route.abort("connectionfailed");
    const offset = Number(url.searchParams.get("offset") || 0);
    if (url.pathname.endsWith("/authority")) return route.fulfill({ json: null });
    if (url.pathname === "/v1/workspace/requests")
      return route.fulfill({
        json: { requests: [], offset: 0, has_more: false, graphs_offset: 0, graphs_has_more: false },
      });
    if (url.pathname === "/v1/controller")
      return route.fulfill({
        json: {
          live_reachable: true,
          serving: "serving",
          role: "held",
          generation_match: "match",
          controller_binding: "d".repeat(64),
          durable_consistency: "consistent",
          work_admission: "open",
        },
      });
    if (url.pathname === "/v1/runs")
      return route.fulfill({
        json: {
          runs: [{ ...run, run_id: offset ? "test-run-50" : run.run_id, agent: "worker-1", executing: false }],
          offset,
          has_more: offset === 0,
        },
      });
    if (url.pathname === "/v1/runs/test-run")
      return route.fulfill({
        json: {
          ...run,
          run_id: mode === "mismatch" ? "wrong-run" : run.run_id,
          ...(mode === "unsafe-source" ? { source: { ...run.source, url: "javascript:alert(1)" } } : {}),
        },
      });
    if (url.pathname.endsWith("/events")) {
      const after = Number(url.searchParams.get("after"));
      const rows = Array.from({ length: 50 }, (_, i) => ({
        sequence: after + i + 1,
        type: `run.event_${after + i + 1}`,
        occurred_at: "2026-10-13T14:00:00Z",
      }));
      return route.fulfill({
        json: {
          run_id: "test-run",
          events: rows,
          next_after: mode === "bad-cursor" ? after : after + 50,
          has_more: after < 9950,
        },
      });
    }
    if (url.pathname === "/v1/workgraphs")
      return route.fulfill({
        json: {
          workgraphs: [{ ...summary, graph_id: offset ? "test-graph-50" : summary.graph_id }],
          offset,
          has_more: offset === 0,
        },
      });
    if (url.pathname === "/v1/workgraphs/test-graph")
      return route.fulfill({
        json: {
          ...summary,
          product,
          units: mode === "bad-dependency" ? [{ ...units[0], depends_on: ["missing"] }] : units,
          frontier: ["unit-1"],
          counts: { total: 1200, completed: 1, awaiting_decision: 1, blocked: 1198 },
        },
      });
    if (url.pathname === "/v1/products")
      return route.fulfill({ json: { products: [product], offset, has_more: false } });
    if (url.pathname === "/v1/products/test-product") {
      const graphsOffset = Number(url.searchParams.get("graphs_offset"));
      return route.fulfill({
        json: {
          ...product,
          configuration: {
            revision: 4,
            policy: { id: "policy-1", revision: "a".repeat(64) },
            context_policy: "limited",
            settings: { environment: "staging" },
          },
          graphs: [summary],
          graphs_offset: graphsOffset,
          graphs_has_more: graphsOffset === 0,
        },
      });
    }
    throw new Error(`Unexpected operational route ${url.pathname}`);
  });
  try {
    await page.setViewportSize({ width: 1440, height: 1080 });
    await page.goto(base + "/app#/runs");
    await page.getByRole("heading", { name: "Runs & activity", exact: true }).waitFor();
    await page.getByRole("button", { name: "Next", exact: true }).click();
    await page.getByText(/offset 50/).waitFor();
    await page.getByRole("button", { name: "Previous", exact: true }).click();
    await page.getByRole("link", { name: "example/payments", exact: false }).last().click();
    await page.getByRole("heading", { name: "Run activity", exact: true }).waitFor();
    assert.equal(
      await page.getByRole("link", { name: "Open recorded source issue", exact: false }).getAttribute("href"),
      "https://example.test/issues/42",
    );
    await page.getByText("50 events · after 0", { exact: true }).waitFor();
    await page.getByRole("button", { name: "Next events", exact: true }).click();
    await page.getByText("50 events · after 50", { exact: true }).waitFor();
    assert.equal(await page.locator(".activity-timeline li").count(), 50, "History does not accumulate unbounded DOM");
    await page.getByRole("button", { name: "Previous events", exact: true }).click();
    await page.getByText("50 events · after 0", { exact: true }).waitFor();
    await page.getByRole("button", { name: "Review stop", exact: true }).click();
    await page.getByRole("dialog").waitFor();
    assert.match(await page.getByRole("dialog").innerText(), /test-run/);
    assert.match(await page.getByRole("dialog").innerText(), /Protected action: stop/);
    await page.keyboard.press("Escape");
    assert.equal(
      await page
        .getByRole("button", { name: "Review stop", exact: true })
        .evaluate((n) => n === document.activeElement),
      true,
    );
    const graphStart = performance.now();
    await page.goto(base + "/app#/workgraphs/test-graph");
    await page.getByRole("heading", { name: "WorkUnit dependency graph", exact: true }).waitFor();
    const graphReadyMs = Math.round(performance.now() - graphStart);
    assert(graphReadyMs < 5000, `1200-unit fixture first page exceeded 5s: ${graphReadyMs}ms`);
    assert.equal(await page.locator(".dependency-map [role=button]").count(), 50);
    await page
      .getByRole("region", { name: "Selected WorkUnit" })
      .getByText("Admitted handoff & producer report", { exact: true })
      .click();
    assert.match(await page.getByRole("region", { name: "Selected WorkUnit" }).innerText(), /grants no authority/);
    assert.equal(await page.locator("main script").count(), 0);
    const second = page.locator(".dependency-map [role=button]").nth(1);
    await second.focus();
    await page.keyboard.press("Enter");
    assert.match(await page.getByRole("region", { name: "Selected WorkUnit" }).innerText(), /Confirm deployment scope/);
    await page.getByRole("button", { name: "Next", exact: true }).click();
    await page.getByText(/50 loaded · offset 50/).waitFor();
    assert.equal(await page.locator(".dependency-map [role=button]").count(), 50);
    await page.goto(base + "/app#/products/test-product");
    await page.getByRole("heading", { name: "Payments platform", exact: true }).waitFor();
    await page.getByRole("button", { name: "Next", exact: true }).click();
    await page.getByText(/offset 50/).waitFor();
    assert.match(await page.locator("main").innerText(), /policy-1/);
    let captures = 0;
    const captureDirectory = resolve(out, "operational-http-fixtures");
    await mkdir(captureDirectory, { recursive: true });
    for (const theme of ["dark", "light"])
      for (const width of [1440, 1920, 390])
        for (const destination of ["runs/test-run", "workgraphs/test-graph", "products/test-product"]) {
          await page.setViewportSize({ width, height: width === 390 ? 844 : 1080 });
          await page.goto(base + "/app#/" + destination);
          await page.getByRole("button", { name: "Refresh", exact: true }).waitFor();
          if ((await page.evaluate(() => document.documentElement.dataset.theme)) !== theme)
            await page.getByRole("button", { name: `Switch to ${theme} theme`, exact: true }).click();
          await page.evaluate(() => {
            document.querySelector(".fixture-label").textContent = "HTTP FIXTURE";
            document.querySelector(".preview-bar > div > span:last-child").textContent =
              "Simulated projection data · presentation checks only";
          });
          assert.equal(
            await page.evaluate(() => document.documentElement.scrollWidth > innerWidth),
            false,
            `${destination} ${width} reflow`,
          );
          assert.deepEqual(
            await accessibilityIssues(page),
            [],
            `${destination} ${width} ${theme} contrast/name checks`,
          );
          await captureFullPage(page, {
            path: resolve(captureDirectory, `${destination.split("/")[0]}-${width}-${theme}.png`),
          });
          captures++;
          if (width === 390 && destination.startsWith("workgraphs")) {
            assert.equal(await page.locator(".dependency-map").isVisible(), false);
            assert.equal(await page.locator(".dependency-mobile .graph-node").count(), 1);
            assert.equal(await page.locator("#workunit-picker option").count(), 50);
          }
        }
    mode = "bad-cursor";
    await page.goto(base + "/app#/runs/test-run");
    await page.getByRole("heading", { name: "Activity unavailable", exact: true }).waitFor();
    mode = "unsafe-source";
    await page.reload();
    await page.getByRole("heading", { name: "Run activity", exact: true }).waitFor();
    assert.equal(
      await page.getByRole("link", { name: "Open recorded source issue", exact: false }).count(),
      0,
      "Untrusted schemes are never links",
    );
    mode = "mismatch";
    await page.reload();
    await page.getByRole("heading", { name: "This view could not be read", exact: true }).waitFor();
    assert.match(await page.locator("main").innerText(), /subject_mismatch/);
    mode = "bad-dependency";
    await page.goto(base + "/app#/workgraphs/test-graph");
    await page.getByRole("heading", { name: "This view could not be read", exact: true }).waitFor();
    assert.match(await page.locator("main").innerText(), /invalid_response/);
    assert.deepEqual(writes, [], "No governed actions were simulated");
    return {
      captures,
      performance: {
        graphUnits: 1200,
        objectiveLogicalIndex: 2000,
        eventLogicalHistory: 10000,
        graphReadyMs,
        renderedNodes: 50,
        renderedEvents: 50,
        boundMs: 5000,
      },
      source: "Explicitly labeled HTTP projection fixtures; no controller acceptance claims",
      checks: [
        "Run and association paging, bounded 10000-event history cursor; changed cursor refused",
        "1200 WorkUnits, 50-node windows, actual dependency edges, mobile inspectors, escaped reports and exact evidence",
        "Exact stop review without submission; keyboard return focus; dark/light desktop/mobile contrast and reflow",
        "Run subject mismatch and missing graph dependency fail closed",
      ],
    };
  } finally {
    page.off("request", watch);
    await page.unroute("**/v1/**");
  }
}
