// Browser interaction checks and visual captures using an already-installed Playwright.
// No package installation, test runner, runtime writes, or production frontend server.
import assert from "node:assert/strict";
import { checkEngineeringApplication } from "./check_engineering_app.mjs";
import { checkOperationalViews } from "./check_operational_views.mjs";
import { checkEngineeringLogin } from "./check_engineering_login.mjs";
import { readFile, mkdir, writeFile } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const modulePath = process.env.ZENCHRON_PLAYWRIGHT_MODULE;
const tokenFile = process.env.ZENCHRON_PREVIEW_TOKEN_FILE;
assert(modulePath && tokenFile, "Set ZENCHRON_PLAYWRIGHT_MODULE and ZENCHRON_PREVIEW_TOKEN_FILE.");
const { chromium } = await import(pathToFileURL(resolve(modulePath)).href);
const base = process.env.ZENCHRON_PREVIEW_URL || "http://127.0.0.1:8787";
assert(["127.0.0.1", "[::1]"].includes(new URL(base).hostname), "Preview must use a loopback listener.");
const out = resolve(process.env.ZENCHRON_PREVIEW_SCREENSHOTS || "docs/design/screenshots");
await mkdir(out, { recursive: true });
const browser = await chromium.launch({
  headless: true,
  ...(process.env.ZENCHRON_CHROME_EXECUTABLE ? { executablePath: process.env.ZENCHRON_CHROME_EXECUTABLE } : {}),
});
const report = { viewports: [], checks: [], accessibility: [], previewFixtureOnly: true };
const errors = [];
const writes = [];
let captures = 0;

async function captureFullPage(page, options) {
  const mobile = page.viewportSize().width <= 760;
  if (mobile)
    await page.evaluate(async () => {
      const sheet = new CSSStyleSheet();
      sheet.replaceSync(
        ".topbar { position: static !important; } .mobile-nav { position: absolute !important; bottom: 0 !important; } body { position: relative !important; }",
      );
      document.adoptedStyleSheets = [...document.adoptedStyleSheets, sheet];
      await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    });
  try {
    await page.screenshot({ ...options, fullPage: true });
  } finally {
    if (mobile)
      await page.evaluate(async () => {
        document.adoptedStyleSheets = document.adoptedStyleSheets.slice(0, -1);
        await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      });
  }
}

try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1080 }, reducedMotion: "reduce" });
  report.login = await checkEngineeringLogin(browser, base, out);
  captures += report.login.captures;
  const page = await context.newPage();
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("request", (request) => {
    if (request.method() !== "GET") writes.push(request.url());
  });
  await page.goto(base + "/design");
  await page.locator("#token").fill((await readFile(tokenFile, "utf8")).trim());
  await page.getByRole("button", { name: "connect", exact: true }).click();
  await page.locator("#preview-state").waitFor();
  await page.getByRole("link", { name: "Objectives", exact: true }).first().click();
  await page.getByRole("searchbox", { name: "Search objectives" }).fill("credential");
  assert.equal(await page.locator("#portfolio-results tbody tr").count(), 1);
  await page.getByLabel("Lifecycle filter").selectOption("Accepted");
  await page.getByRole("heading", { name: "No objectives match these filters" }).waitFor();
  await page.getByRole("button", { name: "Clear filters", exact: true }).click();
  assert.equal(await page.locator("#portfolio-results tbody tr").count(), 10);
  report.checks.push("Search, combined filters, no-results state, filter reset");
  await page.getByRole("link", { name: "Dependency handoff", exact: true }).click();
  assert.match(page.url(), /OBJ-586\/workgraph/);
  await page.goto(base + "/design#/actions/OBJ-586");
  await page.getByRole("heading", { name: "No human decision for this objective" }).waitFor();
  report.checks.push("Dependency blockers navigate to their objective; unmatched decisions never retarget");

  await page.goto(base + "/design#/objectives/OBJ-612");
  await page.locator('[data-unit="WU-614-C"]').click();
  assert.match(await page.locator("#unit-detail").innerText(), /Runnable frontier/);
  for (const [tab, heading] of [
    ["Plan", "EngineeringPlan"],
    ["Evidence", "Applicable evidence"],
    ["Activity", "Causal activity"],
  ]) {
    await page.getByRole("link", { name: tab, exact: true }).click();
    await page.getByRole("heading", { name: heading, exact: true }).waitFor();
  }
  report.checks.push("WorkGraph selection, plan/evidence/activity navigation");

  await page.goto(base + "/design#/actions/OBJ-612");
  const decisionURL = page.url();
  await page.getByRole("button", { name: "Inspect bounded runtime recovery" }).click();
  assert.equal(page.url(), decisionURL);
  assert.equal(await page.locator("#runtime-recovery").evaluate((node) => document.activeElement === node), true);
  await page.getByRole("button", { name: "Review & approve", exact: true }).click();
  await page.getByRole("dialog").waitFor();
  const technical = page.getByRole("dialog").locator("details");
  assert.equal(await technical.evaluate((node) => node.open), false);
  assert.match(await page.getByRole("dialog").innerText(), /DEC-041 · OBJ-612/);
  await technical.locator("summary").focus();
  await page.keyboard.press("Enter");
  assert.match(await page.getByRole("dialog").innerText(), /fixture-controller-g42/);
  assert.match(await page.getByRole("dialog").innerText(), /81e4b9a927c530df268b6a381c591007de52f6a3/);
  for (let i = 0; i < 8; i++) {
    await page.keyboard.press("Tab");
    assert.equal(await page.evaluate(() => Boolean(document.activeElement.closest("dialog"))), true);
  }
  await page.keyboard.press("Escape");
  assert.equal(await page.getByRole("dialog").isVisible(), false);
  assert.equal(
    await page
      .getByRole("button", { name: "Review & approve", exact: true })
      .evaluate((node) => node === document.activeElement),
    true,
  );
  await page.getByRole("button", { name: "Review & approve", exact: true }).click();
  await page.getByLabel("Decision note (optional)").fill("Exact subject inspected <script>fixture note</script>");
  await page.getByRole("button", { name: "Confirm fixture decision", exact: true }).click();
  await page.getByText("Simulated decision already resolved", { exact: true }).waitFor();
  assert.equal(await page.locator("#decision-receipt").evaluate((node) => node === document.activeElement), true);
  assert.equal(await page.locator("[data-decision]").count(), 0);
  assert.match(await page.locator(".decision-detail .decision-subject").innerText(), /OBJ-612/);
  await captureFullPage(page, { path: resolve(out, "actions-resolved-1440.png"), fullPage: true });
  captures++;
  await page.setViewportSize({ width: 390, height: 844 });
  assert.equal(await page.getByLabel("Choose a decision").inputValue(), "OBJ-612");
  assert.equal(await page.getByLabel("Choose a decision").inputValue(), "OBJ-612");
  await captureFullPage(page, { path: resolve(out, "actions-resolved-390.png"), fullPage: true });
  captures++;
  await page.setViewportSize({ width: 1440, height: 1080 });
  await page.locator(".decision-footer").getByRole("link", { name: "Other human requests" }).click();
  assert.equal(await page.locator(".decision-item").count(), 1);
  assert.match(
    await page.getByLabel("Local fixture decision outcomes").innerText(),
    /Exact subject inspected <script>fixture note<\/script>/,
  );
  await page.getByRole("button", { name: "Reset simulation" }).click();
  assert.equal(await page.locator(".decision-item").count(), 2);
  await page.goto(base + "/design#/actions/OBJ-612");
  assert.doesNotMatch(await page.locator(".decision-detail").innerText(), /Fixture verified/);
  assert.equal(await page.locator(".decision-detail .decision-checks .green").count(), 0);
  await page.getByRole("button", { name: "Reject", exact: true }).click();
  assert.match(await page.getByRole("dialog").innerText(), /git.open_pull_request/);
  const reject = page.getByRole("button", { name: "Confirm fixture rejection", exact: true });
  await reject.evaluate((node) => (node.dataset.binding = "changed-binding"));
  await reject.click();
  assert.equal(await page.getByRole("dialog").isVisible(), true);
  assert.match(await page.locator("#announcer").innerText(), /subject changed/);
  await reject.evaluate((node) => (node.dataset.binding = "authority-request-612-r3"));
  await reject.click();
  await page.getByText("Simulated rejection recorded", { exact: true }).waitFor();
  assert.equal(await page.locator("#decision-receipt").evaluate((node) => node === document.activeElement), true);
  assert.equal(await page.locator("[data-decision]").count(), 0);
  await captureFullPage(page, { path: resolve(out, "actions-rejected-1440.png"), fullPage: true });
  captures++;
  await page.setViewportSize({ width: 390, height: 844 });
  assert.equal(await page.getByLabel("Choose a decision").inputValue(), "OBJ-612");
  await captureFullPage(page, { path: resolve(out, "actions-rejected-390.png"), fullPage: true });
  captures++;
  await page.setViewportSize({ width: 1440, height: 1080 });
  await page.getByRole("button", { name: "Reset simulation" }).click();
  report.checks.push(
    "Simulated-only status, exact action confirmation, changed binding refusal, context-preserving rejected/resolved receipts",
  );

  await page.goto(base + "/design#/actions/OBJ-605");
  await page.getByRole("heading", { name: "No human decision for this objective" }).waitFor();
  assert.equal(await page.locator("[data-decision]").count(), 0);
  report.checks.push("Runtime-owned evidence recheck and handoff repair are outside the human inbox");

  await page.goto(base + "/design#/actions/OBJ-612");
  for (const state of ["disconnected", "stale", "permission-refused", "failed-execution"]) {
    await page.getByLabel("Preview state").selectOption(state);
    assert.equal(await page.getByRole("button", { name: "Review & approve", exact: true }).isDisabled(), true);
    assert.equal(await page.getByRole("button", { name: "Reject", exact: true }).isDisabled(), true);
    assert.match(await page.locator(".decision-detail .decision-checks").innerText(), /Unverified/);
    await captureFullPage(page, { path: resolve(out, `actions-${state}-1440.png`), fullPage: true });
    captures++;
    await page.setViewportSize({ width: 390, height: 844 });
    await captureFullPage(page, { path: resolve(out, `actions-${state}-390.png`), fullPage: true });
    captures++;
    await page.setViewportSize({ width: 1440, height: 1080 });
  }
  report.checks.push("Disconnected, stale, permission-refused and failed execution disable decisions");
  await page.goto(base + "/design#/objectives/OBJ-612");
  await page.getByRole("heading", { name: "Execution failure", exact: true }).waitFor();
  assert.match(await page.locator("main").innerText(), /Failed · no automatic restart/);
  await captureFullPage(page, { path: resolve(out, "workspace-failed-execution-1440.png"), fullPage: true });
  captures++;
  await page.getByLabel("Preview state").selectOption("ready");

  await page.goto(base + "/design#/objectives");
  assert.doesNotMatch(await page.locator("#portfolio-results").innerText(), /\d+%/);
  assert.match(await page.locator("#portfolio-results").innerText(), /2 of 4 admitted handoffs/);
  await page.goto(base + "/design#/overview");
  assert.equal(await page.getByRole("region", { name: "Exclusive lifecycle pipeline" }).count(), 0);
  assert.match(await page.locator(".flow-explanation").innerText(), /overlapping observations/);
  assert.match(await page.locator(".metric-strip").innerText(), /Unavailable/);
  assert.match(await page.locator(".capacity-panel").innerText(), /Capacity ceiling unavailable/);
  assert.equal(await page.getByRole("link", { name: /Existing operator console/ }).count(), 0);
  report.checks.push(
    "Grounded counts, overlapping activity, unavailable metrics/capacity and unified preview navigation",
  );

  await page.goto(base + "/design#/objectives");
  await page.getByLabel("Preview state").selectOption("scale");
  assert.equal(await page.locator("#portfolio-results tbody tr").count(), 50);
  assert.match(await page.locator("#portfolio-pages").innerText(), /1–50 of 500/);
  const timings = {};
  let started = performance.now();
  await page.getByRole("button", { name: "Next", exact: true }).click();
  assert.match(await page.locator("#portfolio-pages").innerText(), /51–100 of 500/);
  timings.portfolioPageMs = Math.round(performance.now() - started);
  started = performance.now();
  await page.getByRole("searchbox", { name: "Search objectives", exact: true }).fill("objective 490:");
  assert.equal(await page.locator("#portfolio-results tbody tr").count(), 1);
  timings.portfolioSearchMs = Math.round(performance.now() - started);
  await page.getByRole("button", { name: "Switch to light theme" }).click();
  assert.equal(
    await page.getByRole("searchbox", { name: "Search objectives", exact: true }).inputValue(),
    "objective 490:",
  );
  await page.getByRole("button", { name: "Switch to dark theme" }).click();
  await page.locator("#clear-filters").click();
  await page.goto(base + "/design#/objectives/OBJ-612/activity");
  assert.equal(await page.locator(".activity-timeline li").count(), 50);
  assert.match(await page.locator('[aria-label="activity pagination"]').innerText(), /1–50 of 10000/);
  started = performance.now();
  await page.getByRole("button", { name: "Next", exact: true }).click();
  assert.match(await page.locator('[aria-label="activity pagination"]').innerText(), /51–100 of 10000/);
  assert.equal(await page.locator(".activity-timeline li").count(), 50);
  timings.activityPageMs = Math.round(performance.now() - started);
  assert(
    Object.values(timings).every((ms) => ms < 2000),
    "Fixture interactions must finish within 2 seconds",
  );
  report.scale = {
    objectives: 500,
    events: 10000,
    renderedPageLimit: 50,
    timings,
    includesBrowserAutomationOverhead: true,
  };
  await page.goto(base + "/design#/objectives/OBJ-S490");
  await page
    .getByRole("heading", { name: "Bounded engineering objective 490: preserve exact-subject evidence", exact: true })
    .waitFor();
  report.checks.push(
    "500 objectives / 10000 events, bounded 50-row pages, searchable full fixture index and retained local filters",
  );
  await page.getByLabel("Preview state").selectOption("ready");

  const views = [
    ["overview", "overview"],
    ["portfolio", "objectives"],
    ["workspace", "objectives/OBJ-612"],
    ["actions", "actions/OBJ-612"],
  ];
  for (const width of [1440, 1920, 390]) {
    const height = width === 390 ? 844 : 1080;
    await page.setViewportSize({ width, height });
    for (const [name, route] of views) {
      await page.goto(base + "/design#/" + route);
      await page.locator("main h1").waitFor();
      await page.evaluate(async () => {
        await document.fonts.ready;
        window.scrollTo(0, 0);
        await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      });
      if (name === "actions" && width === 390) {
        assert.equal(
          await page.locator(".mobile-nav").evaluate((node) => Math.round(node.getBoundingClientRect().bottom)),
          height,
        );
        assert.equal(await page.locator(".decision-detail details").evaluate((node) => node.open), false);
        for (const label of ["Reject", "Review & approve"]) {
          const rect = await page.getByRole("button", { name: label, exact: true }).boundingBox();
          assert(
            rect.y >= 0 && rect.y + rect.height <= height - 75,
            `${label} must be on the first mobile screen: ${JSON.stringify(rect)}`,
          );
        }
      }
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
      assert.equal(overflow, false, `${name} overflows at ${width}px`);
      // Fresh tabs avoid Chrome's stale compositor surface after prior full-page captures/resizes.
      const capturePage = await context.newPage();
      capturePage.on("pageerror", (error) => errors.push(error.message));
      try {
        await capturePage.setViewportSize({ width, height });
        await capturePage.goto(base + "/design#/" + route);
        await capturePage.bringToFront();
        await capturePage.locator("main h1").waitFor();
        await capturePage.evaluate(async () => {
          await document.fonts.ready;
          await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
        });
        await capturePage.screenshot({ path: resolve(out, `${name}-${width}-dark.png`), fullPage: false });
        await captureFullPage(capturePage, { path: resolve(out, `${name}-${width}-dark-full.png`) });
      } finally {
        await capturePage.close();
      }
      await page.bringToFront();
      captures += 2;
      report.viewports.push({ name, width, height, theme: "dark", overflow: false });
    }
  }

  // Mobile uses semantic objective records and a persistent bottom navigation.
  await page.goto(base + "/design#/objectives");
  assert.equal(await page.locator(".sidebar").evaluate((node) => node.inert), true);
  await page.getByRole("button", { name: "More navigation" }).click();
  await page.locator(".sidebar.is-open").waitFor();
  for (let i = 0; i < 12; i++) {
    await page.keyboard.press("Tab");
    assert.equal(await page.evaluate(() => Boolean(document.activeElement.closest(".sidebar"))), true);
  }
  await page.getByRole("link", { name: "Execution agents", exact: true }).click();
  await page.getByRole("heading", { name: "Execution agents", exact: true }).waitFor();
  assert.equal(await page.locator(".sidebar").evaluate((node) => node.classList.contains("is-open")), false);
  report.checks.push("Mobile bottom navigation and accessible drawer destinations");
  await page.goto(base + "/design#/actions/OBJ-612");
  await page.getByLabel("Choose a decision").selectOption("OBJ-598");
  await page.getByRole("button", { name: "Review & approve plan", exact: true }).waitFor();
  report.checks.push("Mobile decision chooser navigates to exact plan context");

  await page.setViewportSize({ width: 1440, height: 1080 });
  await page.getByRole("button", { name: "Switch to light theme" }).click();
  for (const [name, route] of views) {
    await page.goto(base + "/design#/" + route);
    await captureFullPage(page, { path: resolve(out, `${name}-1440-light.png`), fullPage: true });
    captures++;
  }
  await page.getByRole("button", { name: "Switch to dark theme" }).click();
  report.checks.push("Dark and light theme switching");

  await page.goto(base + "/design#/overview");
  for (const state of ["empty", "loading", "error", "disconnected", "stale"]) {
    await page.getByLabel("Preview state").selectOption(state);
    await captureFullPage(page, { path: resolve(out, `state-${state}-1440.png`), fullPage: true });
    captures++;
    await page.getByLabel("Preview state").selectOption("ready");
  }
  await page.getByRole("button", { name: "Search objectives" }).click();
  await page.getByRole("searchbox", { name: "Search fixture objectives" }).fill("612");
  await page.locator("[data-search-result]").click();
  assert.match(page.url(), /OBJ-612/);
  report.checks.push("All system states and keyboard-accessible global search");

  for (const width of [360, 720]) {
    await page.setViewportSize({ width, height: 900 });
    for (const [, route] of views) {
      await page.goto(base + "/design#/" + route);
      assert.equal(
        await page.evaluate(() => document.documentElement.scrollWidth > innerWidth),
        false,
        `Reflow at ${width}px`,
      );
      await page.evaluate(() => {
        const sheet = new CSSStyleSheet();
        sheet.replaceSync(
          "p, a, button, label, input, select, textarea, th, td, h1, h2, h3, small, span { line-height: 1.5 !important; letter-spacing: .12em !important; word-spacing: .16em !important; } p { margin-bottom: 2em !important; }",
        );
        document.adoptedStyleSheets = [sheet];
      });
      assert.equal(
        await page.evaluate(() => document.documentElement.scrollWidth > innerWidth),
        false,
        `Text-spacing reflow at ${width}px`,
      );
      await page.evaluate(() => {
        document.adoptedStyleSheets = [];
      });
    }
  }
  report.checks.push("360/720px reflow and enlarged text spacing; confirmation/drawer focus containment and return");
  await page.setViewportSize({ width: 1440, height: 1080 });

  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: width === 390 ? 844 : 1080 });
    await page.goto(base + "/design#/actions/OBJ-612");
    await page.getByRole("button", { name: "Review & approve", exact: true }).click();
    const confirm = page.getByRole("button", { name: "Confirm fixture decision", exact: true });
    const rect = await confirm.boundingBox();
    assert(rect.y >= 0 && rect.y + rect.height <= (width === 390 ? 844 : 1080), "Confirmation stays in the viewport");
    assert.match(await page.getByRole("dialog").innerText(), /DEC-041 · OBJ-612/);
    assert.match(await page.getByRole("dialog").innerText(), /git.open_pull_request/);
    if (width === 390) {
      await page.getByRole("dialog").locator("summary").click();
      const expanded = await confirm.boundingBox();
      assert(expanded.y + expanded.height <= 844, "Expanded binding must not displace confirmation");
      await page.screenshot({ path: resolve(out, "confirmation-390-binding-expanded.png") });
      captures++;
      await page.getByRole("dialog").locator("summary").click();
    }
    await page.screenshot({ path: resolve(out, `confirmation-${width}-dark.png`) });
    captures++;
    await page.keyboard.press("Escape");
  }
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(base + "/design#/actions/OBJ-612");
  await page.locator(".decision-detail summary").click();
  await captureFullPage(page, { path: resolve(out, "actions-390-binding-expanded.png"), fullPage: true });
  captures++;
  assert.match(await page.locator(".decision-detail").innerText(), /81e4b9a927c530df268b6a381c591007de52f6a3/);
  await page.setViewportSize({ width: 1440, height: 1080 });

  // Check actual rendered text contrast and accessible names without adding an audit dependency.
  for (const mode of ["dark", "light"]) {
    if (mode === "light") await page.getByRole("button", { name: "Switch to light theme" }).click();
    for (const [, route] of views) {
      await page.goto(base + "/design#/" + route);
      const issues = await page.evaluate(() => {
        const failures = [];
        const parse = (value) => value.match(/[\d.]+/g)?.map(Number) || [];
        const lum = (channels) =>
          channels
            .slice(0, 3)
            .map((v) => v / 255)
            .map((v) => (v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4))
            .reduce((s, v, i) => s + v * [0.2126, 0.7152, 0.0722][i], 0);
        for (const node of document.querySelectorAll("body *")) {
          const rect = node.getBoundingClientRect();
          if (!rect.width || !rect.height || node.closest("dialog:not([open])") || node.classList.contains("sr-only"))
            continue;
          const style = getComputedStyle(node);
          const text = Array.from(node.childNodes)
            .filter((n) => n.nodeType === Node.TEXT_NODE)
            .map((n) => n.textContent)
            .join("")
            .trim();
          if (text && !["SCRIPT", "STYLE", "OPTION"].includes(node.tagName)) {
            let ancestor = node,
              background = [0, 0, 0, 0];
            while (ancestor && (!background.length || background[3] === 0)) {
              background = parse(getComputedStyle(ancestor).backgroundColor);
              ancestor = ancestor.parentElement;
            }
            const fg = parse(node instanceof SVGElement ? style.fill : style.color),
              a = lum(fg),
              b = lum(background);
            const contrast = (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
            const isLarge =
              parseFloat(style.fontSize) >= 24 ||
              (parseFloat(style.fontSize) >= 18.66 && parseInt(style.fontWeight) >= 700);
            if (contrast < (isLarge ? 3 : 4.5) - 0.01)
              failures.push({ text: text.slice(0, 65), contrast: Number(contrast.toFixed(2)), tag: node.tagName });
          }
          if (node.tagName === "BUTTON" && !node.textContent.trim() && !node.getAttribute("aria-label"))
            failures.push({ name: "Unnamed button" });
        }
        return failures;
      });
      report.accessibility.push({ mode, route, issues });
      assert.deepEqual(issues, [], `${mode} ${route} contrast or accessible-name failures`);
    }
  }
  report.integration = await checkEngineeringApplication(page, base, out, captureFullPage);
  captures += report.integration.captures;
  report.operations = await checkOperationalViews(page, base, out, captureFullPage);
  captures += report.operations.captures;
  assert.deepEqual(errors, [], "Browser script errors");
  assert.deepEqual(writes, [], "Prototype must not make non-GET requests");
  report.checks.push("Rendered text contrast, button names, no horizontal overflow, no script errors, no writes");
  report.captureCount = captures;
  report.captureMethod =
    "Native viewport/dialog images; full-page mobile captures put sticky chrome in document flow to avoid Chrome tile artifacts.";
  await writeFile(resolve(out, "validation.json"), JSON.stringify(report, null, 2) + "\n");
  console.log(
    JSON.stringify(
      {
        captures,
        viewports: report.viewports.length,
        checks: report.checks,
        accessibilityViews: report.accessibility.length,
      },
      null,
      2,
    ),
  );
} finally {
  await browser.close();
}
