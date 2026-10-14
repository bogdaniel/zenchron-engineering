// Presentation boundary checks only. Governed acceptance uses the actual Go/socket test.
import assert from "node:assert/strict";
import { resolve } from "node:path";
import { accessibilityIssues } from "./browser_accessibility.mjs";

export async function checkEngineeringApplication(page, base, out, captureFullPage) {
  const writes = [],
    fixtureModules = [];
  const watch = (req) => {
    if (req.method() !== "GET") writes.push(req.url());
    if (new URL(req.url()).pathname.endsWith("/fixtures.js")) fixtureModules.push(true);
  };
  page.on("request", watch);
  try {
    await page.setViewportSize({ width: 1440, height: 1080 });
    await page.goto(base + "/app#/overview");
    await page.getByRole("heading", { name: "Engineering overview", exact: true }).waitFor();
    assert.match(await page.locator(".preview-bar").innerText(), /Observed API data/);
    assert.doesNotMatch(await page.locator("main").innerText(), /OBJ-612|Fixture verified|81e4b9a/);
    await captureFullPage(page, { path: resolve(out, "runtime-overview-1440.png") });
    await page.setViewportSize({ width: 390, height: 844 });
    await captureFullPage(page, { path: resolve(out, "runtime-overview-390.png") });
    // All mocks start after these genuine isolated empty-state captures.
    let mode = "ready",
      reachable = true,
      reads = 0,
      pending = true;
    const binding = "b".repeat(64),
      digest = "a".repeat(64);
    const row = {
      plan_id: "test-plan",
      repository: "example/repo",
      current_revision: 1,
      source_issue: 41,
      pending: true,
      shown: "proposed",
    };
    const plan = {
      plan_id: row.plan_id,
      revision: 1,
      digest,
      repository: row.repository,
      base_revision: "c".repeat(40),
      governing_revision: 0,
      shown: "proposed",
      stages: [
        { stage_id: "Build <script>text</script>", kind: "agent", depends_on: [], role: "implementer" },
        { stage_id: "Gate", kind: "human_decision_gate", depends_on: ["Build <script>text</script>"] },
      ],
      budget: {
        child_runs: 0,
        max_child_runs: 2,
        max_concurrency: 1,
        wall_seconds: 0,
        provider_invocations: 0,
        max_provider_invocations: 5,
        cost_known: false,
      },
    };
    await page.route("**/v1/**", async (route) => {
      const req = route.request(),
        url = new URL(req.url());
      assert.equal(/^Bearer [a-f0-9]{64}$/.test(req.headers().authorization || ""), true, "Bearer credential required");
      assert.equal(req.method(), "GET", "This boundary check never attempts authority");
      if (mode === "unauthorized") return route.fulfill({ status: 401, json: { error: "unauthorized" } });
      if (mode === "permission") return route.fulfill({ status: 403, json: { error: "forbidden_origin" } });
      if (mode === "failed") return route.abort("connectionfailed");
      if (url.pathname === "/v1/workspace/requests")
        return route.fulfill({
          json: {
            requests: [],
            offset: Number(url.searchParams.get("offset") || 0),
            has_more: false,
            graphs_offset: Number(url.searchParams.get("graphs_offset") || 0),
            graphs_has_more: false,
          },
        });
      if (url.pathname.endsWith("/preview"))
        return route.fulfill({
          json: {
            ...plan,
            plan_id: decodeURIComponent(url.pathname.split("/")[3]),
            objective: "Bounded fixture objective",
            stage_objectives: {},
            stage_rationales: {},
            stage_blockers: {},
            assignments_digest: digest,
            controller_binding: reachable ? binding : "",
            unbound: ["No fixture assignment authority"],
          },
        });
      if (url.pathname === "/v1/fleet")
        return route.fulfill({
          json: {
            capacity: 0,
            executing: 0,
            active: mode === "unavailable-run" ? 1 : 0,
            supervisor_running: reachable,
            counts: {
              active: mode === "unavailable-run" ? 1 : 0,
              waiting: 0,
              failed: 0,
              completed: 0,
              cancelled: 0,
              held: 0,
            },
            classes: {
              working: 0,
              observing: 0,
              runnable: 0,
              waiting: 0,
              paused: 0,
              unavailable: mode === "unavailable-run" ? 1 : 0,
            },
          },
        });
      if (url.pathname.endsWith("/events"))
        return route.fulfill({ json: { plan_id: "test-plan", events: [], next_after: 0, has_more: false } });
      if (url.pathname === "/v1/controller")
        return route.fulfill({
          json: {
            live_reachable: reachable,
            serving: reachable ? "serving" : "unknown",
            controller_binding: reachable ? binding : "",
            role: reachable ? "held" : "",
            work_admission: "unknown",
            durable_consistency: "consistent",
            generation_match: reachable ? "match" : "unknown",
            durable_generation: { kind: "pre_adoption_attested", source_revision: "d".repeat(40) },
          },
        });
      if (url.pathname === "/v1/plans") {
        const offset = Number(url.searchParams.get("offset")),
          query = url.searchParams.get("q");
        const rows = query
          ? [row]
          : Array.from({ length: 50 }, (_, i) => ({
              ...row,
              plan_id: i === 0 && offset === 0 ? row.plan_id : `test-plan-${offset + i}`,
              pending: mode === "paged-pending" ? offset >= 50 && i === 0 : true,
            }));
        return route.fulfill({
          json: mode === "malformed" ? { plans: "bad" } : { plans: rows, offset, has_more: offset < 1950 && !query },
        });
      }
      if (url.pathname.endsWith("/context")) {
        reads++;
        const subject = { ...plan, plan_id: decodeURIComponent(url.pathname.split("/")[3]) };
        return route.fulfill({
          json: {
            plan:
              mode === "mismatch"
                ? { ...subject, plan_id: "other-plan" }
                : mode === "gate-run"
                  ? {
                      ...subject,
                      stages: [
                        { stage_id: "Gate", kind: "human_decision_gate", depends_on: [], run_id: "invented-run" },
                      ],
                    }
                  : subject,
            pending,
            ...(!pending
              ? {
                  receipt: {
                    status: "rejected",
                    revision: 1,
                    digest,
                    sequence: 2,
                    occurred_at: "2026-10-13T14:00:00Z",
                  },
                }
              : {}),
          },
        });
      }
      if (url.pathname === "/v1/runs")
        return route.fulfill({
          json: {
            runs:
              mode === "unavailable-run"
                ? [
                    {
                      run_id: "unreadable-run",
                      phase: "execution",
                      disposition: "active",
                      executing: true,
                      unavailable: true,
                    },
                  ]
                : [],
            offset: 0,
            has_more: false,
          },
        });
      throw new Error("Unexpected read path " + url.pathname);
    });
    await page.setViewportSize({ width: 1440, height: 1080 });
    mode = "unavailable-run";
    await page.reload();
    await page.getByText("Observation unavailable · execution and result unknown").waitFor();
    assert.match(await page.locator(".pipeline-cell").nth(2).innerText(), /≥0[\s\S]*1 unknown/);
    assert.match(
      await page.locator("main").innerText(),
      /0 known executing runs in loaded page · 1 unknown observations/,
    );
    mode = "ready";
    await page.goto(base + "/app#/objectives");
    await page.getByRole("heading", { name: "Objectives", exact: true }).waitFor();
    assert.equal(await page.locator("tbody tr").count(), 50);
    await page.getByRole("button", { name: "Next", exact: true }).click();
    await page.getByText(/offset 50/).waitFor();
    await page.getByRole("searchbox", { name: "Search objectives", exact: true }).fill("example/repo");
    await page.getByRole("button", { name: "Search", exact: true }).click();
    await page.getByText(/1 loaded · offset 0/).waitFor();
    await page.getByRole("link", { name: "Issue #41 · example/repo" }).click();
    await page.getByRole("heading", { name: "Plan dependency inspection" }).waitFor();
    assert.match(await page.getByRole("region", { name: "Selected stage" }).innerText(), /Run not recorded/);
    assert.match(await page.locator("main").innerText(), /Build <script>text<\/script>/);
    assert.equal(await page.locator("main script").count(), 0);
    await page.getByRole("button", { name: /Gate/ }).click();
    assert.match(await page.getByRole("region", { name: "Selected stage" }).innerText(), /Gate creates no run/);
    for (const theme of ["dark", "light"]) {
      if ((await page.evaluate(() => document.documentElement.dataset.theme)) !== theme)
        await page.getByRole("button", { name: `Switch to ${theme} theme`, exact: true }).click();
      for (const route of ["overview", "objectives", "objectives/test-plan", "actions/test-plan/1"]) {
        await page.goto(base + "/app#/" + route);
        await page.getByRole("button", { name: "Refresh", exact: true }).waitFor();
        assert.deepEqual(await accessibilityIssues(page), [], `${theme} ${route} contrast/names`);
      }
    }
    await page.setViewportSize({ width: 390, height: 844 });
    await page.getByRole("button", { name: "More navigation", exact: true }).click();
    for (let i = 0; i < 8; i++) {
      await page.keyboard.press("Tab");
      assert.equal(
        await page.evaluate(() => !!document.activeElement?.closest(".sidebar")),
        true,
        "Mobile drawer contains focus",
      );
    }
    await page.keyboard.press("Escape");
    await page.waitForFunction(() => document.activeElement?.getAttribute("aria-label") === "More navigation");
    assert.equal(
      await page.locator(".sidebar").evaluate((n) => n.inert),
      true,
      "Closed mobile navigation is outside the tab order",
    );
    mode = "paged-pending";
    await page.goto(base + "/app#/actions");
    await page.getByText("0 pending · 0 actionable · plan decisions", { exact: true }).waitFor();
    await page
      .getByRole("group", { name: "Plan request pagination", exact: true })
      .getByRole("button", { name: "Next", exact: true })
      .click();
    await page.getByLabel("Choose a decision").selectOption("test-plan-50/1");
    await page.getByRole("heading", { name: "Review plan revision 1", exact: true }).waitFor();
    assert.match(
      page.url(),
      /actions\/test-plan-50\/1\?offset=50$/,
      "Pending request after the first 50 preserves its exact route and page",
    );
    assert.match(await page.locator(".decision-detail").innerText(), /test-plan-50/);
    mode = "ready";
    await page.goto(base + "/app#/actions/test-plan/1");
    await page.getByRole("heading", { name: "Review plan revision 1" }).waitFor();
    assert.match(await page.locator(".request-summary").innerText(), /50 pending · 50 actionable/);
    await page.clock.install();
    await page.reload();
    await page.getByRole("button", { name: "Refresh", exact: true }).waitFor();
    // Hidden tabs suspend observation; the snapshot must still age out.
    await page.evaluate(() =>
      Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" }),
    );
    await page.clock.fastForward(31000);
    await page.getByText("Snapshot stale · decisions unavailable", { exact: true }).waitFor();
    assert.equal(await page.getByRole("button", { name: "Reject", exact: true }).isDisabled(), true);
    assert.match(await page.locator(".request-summary").innerText(), /50 pending · 0 actionable/);
    await page.evaluate(() =>
      Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" }),
    );
    await page.getByRole("button", { name: "Refresh", exact: true }).click();
    await page.getByText("50 pending · 50 actionable · plan decisions", { exact: true }).waitFor();
    const before = reads;
    await page.getByRole("button", { name: "Reject", exact: true }).click();
    await page.getByRole("dialog").waitFor();
    assert(reads > before, "Review re-reads the exact revision");
    assert.match(await page.getByRole("dialog").innerText(), /Protected action: plan-reject/);
    await page.keyboard.press("Escape");
    await page.getByRole("button", { name: "Runtime recovery" }).click();
    assert.match(page.url(), /actions\/test-plan\/1$/, "Recovery preserves route and subject");
    assert.equal(await page.locator("#runtime-recovery").evaluate((n) => n === document.activeElement), true);
    reachable = false;
    await page.getByRole("button", { name: "Refresh", exact: true }).click();
    await page
      .locator(".decision-footer")
      .getByText("Controller endpoint unreachable or not serving", { exact: true })
      .first()
      .waitFor();
    assert.match(await page.locator(".request-summary").innerText(), /50 pending · 0 actionable/);
    assert.equal(await page.getByRole("button", { name: "Reject", exact: true }).isDisabled(), true);
    await page.goto(base + "/app#/controller");
    await page.getByText("Identity record; does not prove reachability", { exact: true }).waitFor();
    assert.match(await page.locator(".workspace-main .binding-list").innerText(), /Unreachable/);
    reachable = true;
    await page.goto(base + "/app#/actions/test-plan/1");
    await page.getByRole("button", { name: "Reject", exact: true }).waitFor();
    pending = false;
    await page.getByRole("button", { name: "Reject", exact: true }).click();
    await page.getByText("Subject or controller changed · refresh this request", { exact: true }).waitFor();
    assert.equal(await page.getByRole("dialog").count(), 0);
    await page.getByRole("button", { name: "Refresh", exact: true }).click();
    await page.getByRole("heading", { name: "Historical decision receipt", exact: true }).waitFor();
    assert.equal(await page.getByRole("button", { name: "Reject", exact: true }).count(), 0);
    pending = true;
    for (const [state, text] of [
      ["permission", "permission_refused"],
      ["malformed", "invalid_response"],
      ["mismatch", "subject_mismatch"],
      ["gate-run", "invalid_response"],
      ["failed", "Failed to fetch"],
    ]) {
      mode = state;
      await page.getByRole("button", { name: "Refresh", exact: true }).click();
      await page.getByText(`Read unavailable · ${text}`, { exact: true }).waitFor();
      assert.match(await page.locator(".connection-banner").innerText(), new RegExp(text));
      assert.match(await page.locator(".connection-banner").innerText(), /Retained observations are historical/);
    }
    mode = "unauthorized";
    await page.goto(base + "/app#/overview");
    await page.getByRole("heading", { name: "This view could not be read" }).waitFor();
    assert.match(await page.locator("main").innerText(), /unauthorized/);
    assert.doesNotMatch(await page.locator("main").innerText(), /test-plan|OBJ-612/);
    mode = "ready";
    await page.goto(base + "/app#/objectives/%XX");
    await page.getByRole("heading", { name: "No supported destination" }).waitFor();
    assert.deepEqual(writes, []);
    assert.deepEqual(fixtureModules, []);
    return {
      captures: 2,
      httpMocksOnlyAfterRealCaptures: true,
      checks: [
        "Genuine isolated empty API captures; React app never imports fixtures",
        "2000-objective logical paged HTTP fixture; full-index search; gate/run distinction; escaped text",
        "Pending versus actionable; identity independent of reachability; changed request refused before confirmation",
        "Historical receipt, route-preserving recovery, refused/malformed/mismatched/disconnected reads retain stale context",
        "Eight dark/light contrast/name checks; no writes in this boundary-only suite",
      ],
    };
  } catch (error) {
    await page.screenshot({ path: resolve(out, "react-boundary-failure.png"), fullPage: true });
    throw error;
  } finally {
    page.off("request", watch);
    await page.unroute("**/v1/**");
  }
}
