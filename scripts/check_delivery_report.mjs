// Acceptance driver for the objective delivery report (#693), started by
// TestDeliveryReportAcceptance. It was written from the issue and fails until
// the feature exists. A real browser takes the rehearsal's controlled objective
// (two parallel producers, an integrator, an independent reviewer, one human
// decision) until the producers and the integration are complete while the
// review still awaits human publication authority, then reads the report the
// operator would read on the Objective Workspace outcome view and its JSON
// export. Everything it asserts is stated by the issue; the markup it relies on
// is only roles and accessible names, so any implementation that is accessible
// and truthful satisfies it.
import assert from "node:assert/strict";
import { readFile, mkdir, writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import { resolve } from "node:path";
import { accessibilityIssues } from "./browser_accessibility.mjs";

const { chromium } = await import(pathToFileURL(process.env.ZENCHRON_PLAYWRIGHT_MODULE).href);
const base = process.env.ZENCHRON_E2E_URL,
  out = process.env.ZENCHRON_E2E_ARTIFACTS || "/private/tmp/zenchron-delivery-report",
  gates = process.env.ZENCHRON_REHEARSAL_GATES;
const token = (await readFile(process.env.ZENCHRON_E2E_TOKEN_FILE, "utf8")).trim();
assert(/^http:\/\/127\.0\.0\.1:\d+$/.test(base));
await mkdir(out, { recursive: true });

const browser = await chromium.launch({
  headless: true,
  executablePath:
    process.env.ZENCHRON_CHROME_EXECUTABLE || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
});
const page = await (await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: "reduce" })).newPage();
const errors = [],
  posts = [];
page.on("pageerror", (e) => errors.push(e.message));
page.on("request", (r) => {
  if (r.method() === "POST") posts.push(new URL(r.url()).pathname);
});

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const main = async () => (await page.locator("main").innerText()).replace(/[ \t]+/g, " ");
const capture = (name) => page.screenshot({ path: resolve(out, name + ".png"), fullPage: true });
const visit = async (destination) => {
  await page.goto(base + "/app#/" + destination);
  await page.getByRole("heading", { level: 1 }).first().waitFor();
  await sleep(700);
};
const confirm = (command) =>
  page
    .getByRole("dialog")
    .getByRole("button", { name: "Confirm " + command, exact: true })
    .click();
// The workspace polls on a long cadence; an operator reloads to observe.
const waitText = async (pattern, timeout) => {
  const end = Date.now() + timeout;
  while (Date.now() < end) {
    if (pattern.test(await main())) return;
    await sleep(1000);
    await page.reload();
    await page.getByRole("heading", { level: 1 }).first().waitFor();
    await sleep(500);
  }
  assert.fail(`never saw ${pattern}; the page held: ${(await main()).slice(0, 400)}`);
};
const authorized = (path) => fetch(base + path, { headers: { Authorization: "Bearer " + token } });
const keysOf = (value, found = new Set()) => {
  if (Array.isArray(value)) value.forEach((v) => keysOf(v, found));
  else if (value && typeof value === "object")
    for (const [k, v] of Object.entries(value)) {
      found.add(k);
      keysOf(v, found);
    }
  return found;
};
const withoutClock = (value) =>
  JSON.parse(JSON.stringify(value, (k, v) => (/^(generated_at|generated|exported_at)$/.test(k) ? undefined : v)));

let failure = "";
try {
  await page.goto(base + "/app#/products");
  await page.getByLabel("Control Plane token", { exact: true }).fill(token);
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await page.getByRole("heading", { level: 1 }).first().waitFor();
  await sleep(700);

  // The journey: one objective, an approved plan, a human decision, units done.
  await visit("intake");
  await page.getByLabel("Product", { exact: true }).selectOption({ label: "storefront" });
  await page.getByLabel("Existing issue", { exact: true }).fill("41");
  await page.getByLabel("Plan template", { exact: true }).selectOption("rehearsal-team");
  await page.getByLabel("Use deterministic planning", { exact: true }).check();
  await page.getByRole("button", { name: "Review objective proposal", exact: true }).click();
  await page.getByRole("dialog").waitFor();
  await confirm("plan revise");
  await page.getByRole("heading", { name: "Plan dependency inspection", exact: true }).waitFor({ timeout: 30000 });
  const plan = decodeURIComponent(page.url().split("/objectives/")[1] || "").split("/")[0];
  assert(plan, "the proposed objective plan has no id");
  await page.getByRole("button", { name: "Review & approve", exact: true }).click();
  await page.getByRole("dialog").waitFor();
  await confirm("plan approve");
  await page.getByRole("heading", { name: "Historical plan approval receipt", exact: true }).waitFor({ timeout: 30000 });

  await visit("workgraphs");
  await page.locator("main a[href*='workgraphs/']").first().click();
  await page.getByRole("heading", { level: 1 }).first().waitFor();
  await sleep(1500);
  const graph = decodeURIComponent(page.url().split("/workgraphs/")[1] || "");
  assert(graph, "the approved plan bound no WorkGraph");

  await writeFile(resolve(gates, "go-frontend"), "go\n");
  await writeFile(resolve(gates, "go-backend"), "go\n");
  await visit("actions");
  await waitText(/migration strategy/i, 90000);
  await page.locator("[data-request-id]").first().click();
  await sleep(500);
  await page.getByLabel("Requested outcome", { exact: true }).selectOption("approach-b");
  await page.getByRole("button", { name: "Review decision", exact: true }).click();
  await page.getByRole("dialog").waitFor();
  await confirm("decision resolve");
  await page.getByRole("heading", { name: "Historical decision receipt", exact: true }).waitFor({ timeout: 30000 });
  await visit("workgraphs/" + encodeURIComponent(graph));
  await waitText(/completed\s*\n?\s*3/, 120000);

  // The report, on the existing Objective Workspace outcome view.
  await visit("objectives/" + encodeURIComponent(plan));
  const heading = page.getByRole("heading", { name: "Delivery report", level: 2 });
  await heading.waitFor({ timeout: 30000 });
  const report = page.getByRole("region", { name: "Delivery report" });
  await report.waitFor();
  await capture("01-report");
  const postsBeforeReading = posts.length;
  const reportText = (await report.innerText()).replace(/[ \t]+/g, " ");

  // Completed work is shown separately from published work, and nothing is
  // called published or given a pull request that does not exist.
  const completed = report.getByRole("region", { name: "Completed work" });
  const completedText = await completed.innerText();
  for (const unit of ["backend", "frontend", "integrate"])
    assert(completedText.includes(unit), `completed work does not name the ${unit} unit: ${completedText}`);
  const published = report.getByRole("region", { name: "Published work" });
  const publishedText = await published.innerText();
  assert.match(publishedText, /not published|nothing (has been )?published/i, publishedText);
  assert.match(publishedText, /no pull request/i, publishedText);
  assert.equal(await published.getByRole("link", { name: /pull request|#\d+/i }).count(), 0, "a pull request was invented");

  // What is still outstanding is named: the independent review and the human
  // authority to publish.
  const outstanding = await report.getByRole("region", { name: "Outstanding" }).innerText();
  assert.match(outstanding, /review/i, outstanding);
  assert.match(outstanding, /publication authority/i, outstanding);

  // Who contributed: both agents and their providers, from recorded state.
  assert.match(reportText, /codex/, "the producing agent is not named");
  assert.match(reportText, /claude/, "the reviewing agent is not named");

  // An unmeasured field is labelled unknown, never zero or invented.
  const measurements = await report.getByRole("region", { name: "Measurements" }).innerText();
  assert.match(measurements, /tokens/i, measurements);
  assert.match(measurements, /cost/i, measurements);
  assert.match(measurements, /unknown|not recorded/i, measurements);
  assert(!/\$\s?\d|\b0\s+tokens\b|\bcost[:\s]+0\b/i.test(measurements), "a missing measurement was shown as a number: " + measurements);

  // Accessible: named controls only, and the whole page passes the shared checks.
  for (const control of await report.getByRole("link").all())
    assert((await control.innerText()).trim() || (await control.getAttribute("aria-label")), "an unnamed link in the report");
  const issues = await accessibilityIssues(page);
  assert.deepEqual(issues, [], "accessibility issues on the outcome view: " + JSON.stringify(issues));

  // The export: bounded, typed, free of secrets, transcripts and provider context.
  const exportLink = report.getByRole("link", { name: /export.*json/i });
  assert.equal(await exportLink.count(), 1, "the report offers no JSON export");
  const href = await exportLink.getAttribute("href");
  assert(href && !/^https?:\/\//.test(href), "the export must be a same-origin API route: " + href);
  const first = await authorized(href);
  assert.equal(first.status, 200, "export status");
  assert.match(first.headers.get("content-type") || "", /application\/json/);
  const body = await first.text();
  assert(body.length <= 256 * 1024, `the export is unbounded: ${body.length} bytes`);
  assert(!body.includes(token), "the export contains the Control Plane token");
  const exported = JSON.parse(body);
  assert.equal(typeof exported.schema_version, "string", "the export is not versioned");
  for (const key of ["objective", "work_units", "publication", "measurements"])
    assert(key in exported, `the export lacks ${key}`);
  assert(Array.isArray(exported.work_units) && exported.work_units.length >= 3, "the export lists fewer than the three completed units");
  const forbidden = [...keysOf(exported)].filter((k) => /^(token|access_token|auth_token|bearer|authorization)$|transcript|provider_context|prompt|secret|credential|password|api_key/i.test(k));
  assert.deepEqual(forbidden, [], "the export carries private material: " + forbidden.join(", "));

  // Every claim is source-backed: each run of the WorkGraph appears by exact id.
  const graphView = await (await authorized("/v1/workgraphs/" + encodeURIComponent(graph))).text();
  const runs = [...new Set(graphView.match(/run-[0-9a-f]{16,}/g) || [])];
  assert(runs.length >= 3, "the WorkGraph names fewer than three runs: " + runs.length);
  for (const run of runs) assert(body.includes(run), `the export does not cite ${run}`);
  assert(body.includes(graph) || reportText.includes(graph), "the report does not cite the WorkGraph by id");

  // Reading is not acting, and the same records give the same report.
  const second = await (await authorized(href)).json();
  assert.deepEqual(withoutClock(second), withoutClock(exported), "two reads of the same records differ");
  await page.reload();
  await heading.waitFor();
  assert.equal(await completed.innerText(), completedText, "a reload changed the completed work");
  assert.deepEqual(posts.slice(postsBeforeReading), [], "reading the report performed an action");
  await capture("02-report-after-reload");
} catch (error) {
  failure = String(error.message).split("\n").slice(0, 3).join(" | ");
  try {
    await capture("99-failure");
  } catch {}
} finally {
  await browser.close();
}
if (errors.length && !failure) failure = "page errors: " + errors.join(" | ");
if (failure) {
  console.error("FAIL: " + failure);
  process.exit(1);
}
console.log("PASS: the delivery report states what was delivered, by whom, against what, and what remains unproven");
