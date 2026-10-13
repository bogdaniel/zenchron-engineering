// Invoked by TestEngineeringOSPlanRejectEndToEnd against its isolated real
// controller, socket, role and journal. Never points at an operator's store.
import assert from "node:assert/strict";
import { readFile, mkdir, writeFile } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const base = process.env.ZENCHRON_LIVE_URL;
assert(base && new URL(base).hostname === "127.0.0.1", "An isolated loopback test listener is required");
const { chromium } = await import(pathToFileURL(process.env.ZENCHRON_PLAYWRIGHT_MODULE).href);
const browser = await chromium.launch({
  headless: true,
  ...(process.env.ZENCHRON_CHROME_EXECUTABLE ? { executablePath: process.env.ZENCHRON_CHROME_EXECUTABLE } : {}),
});
const phase = process.env.ZENCHRON_LIVE_PHASE;
const losesReply = phase === "reject-lost-reply";
const out = resolve(process.env.ZENCHRON_LIVE_SCREENSHOTS);
const id = process.env.ZENCHRON_LIVE_PLAN;
const report = {
  source: "Isolated test data, actual controller/socket/role/journal; no production fleet measurements",
  phase,
  checks: [],
  captures: [],
};
const failures = [];
try {
  await mkdir(out, { recursive: true });
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, reducedMotion: "reduce" });
  await context.addCookies([
    {
      name: "zenchron_control_plane_token",
      value: (await readFile(process.env.ZENCHRON_LIVE_TOKEN_FILE, "utf8")).trim(),
      url: base,
    },
  ]);
  let page;
  let viewport = { width: 390, height: 844 };
  const writes = [];
  const destination = `actions/${encodeURIComponent(id)}/1`;
  async function visit(route) {
    if (page) await page.close();
    page = await context.newPage();
    await page.setViewportSize(viewport);
    page.on("pageerror", (e) => failures.push(e.message));
    page.on("request", (req) => {
      if (req.method() !== "GET") writes.push(req);
    });
    await page.goto(`${base}/app#/${route}`);
    await page.getByRole("button", { name: "Refresh", exact: true }).waitFor();
    await page.evaluate(() => scrollTo(0, 0));
    await page.evaluate(() => {
      document.querySelector(".topbar-product").textContent = "Engineering OS · isolated test data";
    });
  }
  async function capture(name) {
    const filename = `${name}.png`;
    await page.screenshot({ path: resolve(out, filename) });
    report.captures.push(filename);
  }
  await visit(destination);
  if (phase.startsWith("reject")) {
    assert.match(await page.locator(".request-summary").innerText(), /1 pending · 1 actionable/);
    assert.equal(await page.getByRole("button", { name: "Review & approve", exact: true }).isDisabled(), true);
    assert.equal(await page.locator(".decision-detail details[open]").count(), 0);
    const reject = page.getByRole("button", { name: "Reject", exact: true });
    const box = await reject.boundingBox();
    assert(
      box.y + box.height <= 779,
      `Mobile decision controls must fit above bottom navigation: ${JSON.stringify(box)}`,
    );
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
    report.checks.push("390px exact subject, visible actions, collapsed binding; unsupported approval disabled");
    for (const theme of losesReply ? [] : ["dark", "light"]) {
      await page.evaluate((t) => localStorage.setItem("zenchron-theme", t), theme);
      for (const width of [1440, 1920, 390]) {
        viewport = { width, height: width === 390 ? 844 : 1080 };
        for (const [name, route] of [
          ["overview", "overview"],
          ["portfolio", "objectives"],
          ["workspace", `objectives/${encodeURIComponent(id)}`],
          ["actions", destination],
        ]) {
          await visit(route);
          assert.equal(
            await page.evaluate(() => document.documentElement.dataset.theme),
            theme,
            "Capture actual selected theme",
          );
          assert.equal(
            await page.evaluate(() => document.documentElement.scrollWidth > innerWidth),
            false,
            `${name} ${width} reflow`,
          );
          await capture(`${name}-${width}-${theme}`);
        }
      }
    }
    await page.evaluate(() => localStorage.setItem("zenchron-theme", "dark"));
    viewport = { width: 390, height: 844 };
    await visit(destination);
    const currentReject = page.getByRole("button", { name: "Reject", exact: true });
    await currentReject.click();
    await page.getByRole("dialog").waitFor();
    assert.match(await page.getByRole("dialog").innerText(), /Protected action: plan-reject/);
    assert.match(await page.getByRole("dialog").innerText(), new RegExp(id));
    const confirm = page.getByRole("button", { name: "Confirm rejection", exact: true });
    for (let i = 0; i < 12; i++) {
      await page.keyboard.press("Tab");
      assert.equal(
        await page.evaluate(() => !!document.activeElement?.closest("dialog")),
        true,
        "Native modal contains keyboard focus",
      );
    }
    await page.keyboard.press("Escape");
    assert.equal(
      await currentReject.evaluate((node) => node === document.activeElement),
      true,
      "Cancelled confirmation returns to exact opener",
    );
    await currentReject.click();
    await page.getByRole("dialog").locator("summary").click();
    const expanded = await confirm.boundingBox();
    assert(expanded.y + expanded.height <= 844, "Expanded binding does not displace confirmation");
    await capture("confirmation-390-expanded");
    await page.getByRole("dialog").locator("summary").click();
    if (losesReply)
      await page.route("**/v1/plans/*/reject", async (route) => {
        const response = await route.fetch();
        assert.equal((await response.json()).outcome, "applied", "Actual controller applied before the response fault");
        await route.abort("connectionfailed");
      });
    await confirm.click();
    if (losesReply) {
      await page
        .getByText("Outcome unknown · re-read the exact revision before any further action", { exact: true })
        .waitFor();
      assert.equal(await page.getByRole("button", { name: "Reject", exact: true }).isDisabled(), true);
      assert.match(await page.locator(".request-summary").innerText(), /1 pending · 0 actionable/);
      await capture("unknown-outcome-390");
      await page.route("**/v1/plans/*/context?*", (route) => route.abort("connectionfailed"));
      await page.getByRole("button", { name: "Reconcile exact revision", exact: true }).click();
      await page.getByText("Reconciliation unavailable · no retry was submitted", { exact: true }).waitFor();
      assert.equal(await page.getByRole("button", { name: "Reject", exact: true }).isDisabled(), true);
      await page.unroute("**/v1/plans/*/context?*");
      await page.getByRole("button", { name: "Reconcile exact revision", exact: true }).click();
      report.checks.push(
        "Response discarded after actual journal write; unknown outcome disables action; failed read stays closed; reconciliation never resends",
      );
    }
    await page.getByRole("heading", { name: "Historical decision receipt", exact: true }).waitFor();
    await page.waitForFunction(() => document.activeElement?.id === "decision-receipt");
    assert.match(await page.locator(".decision-detail").innerText(), /Plan rejected · r1/);
    assert.equal(writes.length, 1, "Exactly one POST, no automatic resubmission");
    const body = writes[0].postDataJSON();
    assert.equal(body.revision, 1);
    assert.match(body.digest, /^[a-f0-9]{64}$/);
    assert.match(body.controller_binding, /^[a-f0-9]{64}$/);
    assert.match(page.url(), new RegExp(destination), "Receipt retains the exact selected revision");
    await capture("receipt-390-dark");
    report.checks.push(
      "Fresh review reads; exact digest/controller POST; real governed rejection; durable receipt; focus retained; one submission",
    );
  } else {
    await page.getByRole("heading", { name: "Historical decision receipt", exact: true }).waitFor();
    assert.equal(
      await page.getByRole("button", { name: "Reject", exact: true }).count(),
      0,
      "No action on historical receipt",
    );
    assert.match(await page.locator(".request-summary").innerText(), /0 pending · 0 actionable/);
    await capture(`receipt-${phase}-390`);
    await visit("controller");
    assert.match(await page.locator(".binding-list").innerText(), /Identity record; does not prove reachability/);
    assert.match(
      await page.locator(".binding-list").innerText(),
      phase === "disconnected" ? /Unreachable/ : /Reachable at read time/,
    );
    await capture(`controller-${phase}-390`);
    assert.equal(writes.length, 0, "Reload/reconnect must never resubmit a decision");
    report.checks.push(
      "Reopened read store and controller socket/role; historical receipt recovered; independent identity/reachability; no mutation",
    );
  }
  assert.deepEqual(failures, []);
  await writeFile(resolve(out, `${phase}-validation.json`), JSON.stringify(report, null, 2) + "\n");
  console.log(JSON.stringify({ phase, checks: report.checks, captures: report.captures.length }));
} finally {
  await browser.close();
}
