// #481 operator-usability rehearsal driver. Drives the Product Workspace in a
// real browser against the isolated controller started by
// TestOperatorRehearsal (cmd/zenchron-engineering/operator_rehearsal_test.go).
// Every step records PASS / PARTIAL / FAIL with the exact user-visible
// evidence; a failing step never aborts the rest, and nothing is inferred
// from source. Screenshots show the application only after login, so the
// Control Plane token is never captured.
import { readFile, mkdir, writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import { resolve } from "node:path";
const { chromium } = await import(pathToFileURL(process.env.ZENCHRON_PLAYWRIGHT_MODULE).href);
const base = process.env.ZENCHRON_E2E_URL,
  out = process.env.ZENCHRON_E2E_ARTIFACTS,
  gates = process.env.ZENCHRON_REHEARSAL_GATES;
const token = (await readFile(process.env.ZENCHRON_E2E_TOKEN_FILE, "utf8")).trim();
await mkdir(out, { recursive: true });
const browser = await chromium.launch({
  headless: true,
  executablePath: process.env.ZENCHRON_CHROME_EXECUTABLE || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
});
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: "reduce" });
const page = await context.newPage();
const results = [],
  posts = [],
  pageErrors = [];
page.on("pageerror", (e) => pageErrors.push(e.message));
page.on("request", (r) => {
  if (r.method() === "POST") posts.push(new URL(r.url()).pathname);
});
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const main = async () => (await page.locator("main").innerText()).replace(/[ \t]+/g, " ");
const visit = async (destination) => {
  await page.goto(base + "/app#/" + destination);
  await page.getByRole("heading", { level: 1 }).first().waitFor();
  await sleep(700);
};
// Screenshot + text dump of what the operator sees. The token must never be
// on screen; fail loudly if it ever is.
const snap = async (name, fullPage = false) => {
  const text = await page.locator("body").innerText();
  if (text.includes(token)) throw new Error("token visible on screen in " + name);
  await page.screenshot({ path: resolve(out, name + ".png"), fullPage });
  await writeFile(resolve(out, name + ".txt"), (await main()) + "\n");
  return name + ".png";
};
const waitText = async (pattern, timeout = 60000) => {
  const end = Date.now() + timeout;
  while (Date.now() < end) {
    if (pattern.test(await main())) return true;
    await sleep(1000);
    // The workspace polls on a long cadence; an operator reloads to observe.
    await page.reload();
    await page.getByRole("heading", { level: 1 }).first().waitFor();
    await sleep(500);
  }
  return false;
};
let current;
const step = async (id, title, fn) => {
  current = { id, title, status: "PASS", evidence: [], notes: [], owner: "" };
  console.log(`--- step ${id}: ${title}`);
  try {
    await fn(current);
  } catch (error) {
    current.status = "FAIL";
    current.notes.push("script/UI error: " + String(error.message).split("\n")[0]);
    try {
      current.evidence.push(await snap(`${String(id).padStart(2, "0")}-failure`));
    } catch {}
  }
  results.push(current);
  console.log(`${current.status} step ${id}: ${current.notes.join(" ; ")}`);
};
const confirm = async (command) =>
  page
    .getByRole("dialog")
    .getByRole("button", { name: "Confirm " + command, exact: true })
    .click();
const release = (stage) => writeFile(resolve(gates, "go-" + stage), "go\n");

let plan = "",
  graph = "",
  product = "";
try {
  await page.goto(base + "/app#/products");
  await page.getByLabel("Control Plane token", { exact: true }).fill(token);
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await page.getByRole("heading", { level: 1 }).first().waitFor();
  await sleep(700);

  await step(1, "Open and select the product", async (s) => {
    s.evidence.push(await snap("01-products"));
    const link = page.getByRole("link", { name: /storefront/ }).first();
    await link.click();
    await page.getByRole("heading", { level: 1 }).first().waitFor();
    await sleep(800);
    product = decodeURIComponent(page.url().split("/products/")[1] || "");
    s.evidence.push(await snap("01-product-workspace"));
    s.notes.push("product page: " + (await main()).slice(0, 300).replace(/\n+/g, " | "));
    if (!product) {
      s.status = "FAIL";
      s.notes.push("selecting the product did not open a product workspace route");
    }
  });

  await step(2, "Create an objective for the product", async (s) => {
    await visit("intake");
    await page.getByLabel("Product", { exact: true }).selectOption({ label: "storefront" });
    await page.getByLabel("Existing issue", { exact: true }).fill("41");
    await page.getByLabel("Plan template", { exact: true }).selectOption("rehearsal-team");
    await page.getByLabel("Use deterministic planning", { exact: true }).check();
    s.evidence.push(await snap("02-intake-filled"));
    await page.getByRole("button", { name: "Review objective proposal", exact: true }).click();
    await page.getByRole("dialog").waitFor();
    s.evidence.push(await snap("02-intake-confirm"));
    await confirm("plan revise");
    await page.getByRole("heading", { name: "Plan dependency inspection", exact: true }).waitFor({ timeout: 30000 });
    plan = decodeURIComponent(page.url().split("/objectives/")[1] || "").split("/")[0];
    s.evidence.push(await snap("02-objective-created"));
    s.notes.push("plan id " + plan);
  });

  await step(3, "Inspect the proposed plan (stages, profiles, dependencies, budget)", async (s) => {
    s.evidence.push(await snap("03-proposed-plan", true));
    const text = await main();
    for (const want of ["backend", "frontend", "integrate", "review"])
      if (!text.includes(want)) {
        s.status = "FAIL";
        s.notes.push("stage " + want + " not shown on the proposed plan");
      }
    s.notes.push("shows profiles: " + /rehearsal-producer-a/.test(text) + ", agents: " + /codex|claude/.test(text));
  });

  await step(4, "Approve the plan through the governed UI action", async (s) => {
    await page.getByRole("button", { name: "Review & approve", exact: true }).click();
    await page.getByRole("dialog").waitFor();
    s.evidence.push(await snap("04-approve-confirm"));
    await confirm("plan approve");
    await page.getByRole("heading", { name: "Historical plan approval receipt", exact: true }).waitFor({ timeout: 30000 });
    s.evidence.push(await snap("04-approved"));
  });

  await step(5, "Inspect the WorkGraph", async (s) => {
    await visit("workgraphs");
    s.evidence.push(await snap("05-workgraphs"));
    const link = page.locator("main a[href*='workgraphs/']").first();
    await link.click();
    await page.getByRole("heading", { level: 1 }).first().waitFor();
    await sleep(1500);
    graph = decodeURIComponent(page.url().split("/workgraphs/")[1] || "");
    s.evidence.push(await snap("05-workgraph-detail", true));
    s.notes.push("graph " + graph);
  });

  await step(6, "Understand assignments, dependencies and concurrent execution", async (s) => {
    await visit("objectives/" + encodeURIComponent(plan));
    s.evidence.push(await snap("06-objective-assignments", true));
    const objective = await main();
    s.notes.push("objective page names assigned agent codex: " + /codex/.test(objective) + "; claude: " + /claude/.test(objective));
    await page.getByLabel("Inspect stage", { exact: true }).selectOption({ label: "review · agent" });
    await sleep(500);
    s.evidence.push(await snap("06-review-stage-assignment"));
    const reviewStage = await main();
    s.notes.push("review stage assigned to claude / independent: " + /claude/.test(reviewStage) + " / " + /independen/i.test(reviewStage));
    await visit("workgraphs/" + encodeURIComponent(graph));
    s.evidence.push(await snap("06-graph-concurrent", true));
    const text = await main();
    const running = /running\s*\n?\s*2/.test(text) || /Executing \/ capacity\s*\n?\s*2/.test(text);
    s.notes.push("two units shown running together: " + running);
    if (!running) s.status = "PARTIAL";
  });

  await step(7, "Inspect a WorkUnit's budget and attempts (#612 panel)", async (s) => {
    await page.getByText("Budget observations", { exact: true }).click();
    await sleep(800);
    s.evidence.push(await snap("07-budget-attempts", true));
    s.notes.push("panel text: " + (await main()).split("Budget observations")[1]?.slice(0, 700).replace(/\n+/g, " | "));
  });

  await writeFile(resolve(gates, "go-frontend"), "go\n");
  await writeFile(resolve(gates, "go-backend"), "go\n");

  await step(8, "Read an admitted handoff (frontend) and the unit's evidence", async (s) => {
    await visit("workgraphs/" + encodeURIComponent(graph));
    const found = await waitText(/completed\s*\n?\s*1|handoff/i, 90000);
    await sleep(1000);
    s.evidence.push(await snap("08-graph-after-frontend", true));
    s.notes.push("frontend completion visible: " + found);
    if (!found) s.status = "FAIL";
  });

  await step(9, "Find the open human DecisionRequest", async (s) => {
    await visit("actions");
    const found = await waitText(/migration strategy/i, 90000);
    s.evidence.push(await snap("09-action-required", true));
    if (!found) {
      s.status = "FAIL";
      s.notes.push("the worker's open decision never appeared in Action required");
    }
  });

  await step(10, "Resolve the decision through the authorized UI action", async (s) => {
    await page.locator("[data-request-id]").first().click();
    await sleep(500);
    s.evidence.push(await snap("10-decision-open", true));
    await page.getByLabel("Requested outcome", { exact: true }).selectOption("approach-b");
    await page.getByRole("button", { name: "Review decision", exact: true }).click();
    await page.getByRole("dialog").waitFor();
    s.evidence.push(await snap("10-decision-confirm"));
    await confirm("decision resolve");
    await page.getByRole("heading", { name: "Historical decision receipt", exact: true }).waitFor({ timeout: 30000 });
    s.evidence.push(await snap("10-decision-resolved", true));
  });

  await step(11, "Observe the dependent WorkUnit become runnable and the backend resume", async (s) => {
    await visit("workgraphs/" + encodeURIComponent(graph));
    const found = await waitText(/completed\s*\n?\s*3/, 120000);
    s.evidence.push(await snap("11-graph-all-complete", true));
    s.notes.push("all three units completed: " + found);
    if (!found) s.status = "FAIL";
  });

  await step("11b", "Diagnose: every run's state", async (s) => {
    await visit("runs");
    s.evidence.push(await snap("11b-runs", true));
    const hrefs = await page.locator("main a[href*='runs/']").evaluateAll((a) => [...new Set(a.map((x) => x.getAttribute("href")))]);
    let i = 0;
    for (const href of hrefs.slice(0, 6)) {
      await page.goto(base + "/app" + href);
      await page.getByRole("heading", { level: 1 }).first().waitFor();
      await sleep(900);
      s.evidence.push(await snap("11b-run-" + i++, true));
    }
  });

  await step(14, "Find and authorize the outstanding publication request, then locate the PR", async (s) => {
    await visit("actions");
    s.evidence.push(await snap("14-actions-publication", true));
    const cards = await page.locator("[data-request-id]").count();
    s.notes.push("Action required publication cards while the review stage's own run awaits human authority: " + cards);
    if (!cards) s.status = "PARTIAL";
    await visit("runs");
    const row = page.locator("main tr, main [role=row]").filter({ hasText: "claude" }).first();
    await row.locator("a[href*='runs/']").first().click();
    await page.getByRole("heading", { level: 1 }).first().waitFor();
    await sleep(900);
    s.evidence.push(await snap("14-review-run", true));
    const button = page.getByRole("button", { name: "Review & authorize publication", exact: true });
    if (await button.isDisabled()) {
      s.status = "FAIL";
      s.notes.push("the run awaiting authority offers no enabled publication action: " + (await main()).match(/No outstanding human publication request|Publication authority[^\n]*\n[^\n]*/)?.[0]);
      return;
    }
    await button.click();
    await page.getByRole("dialog").waitFor();
    s.evidence.push(await snap("14-publication-confirm"));
    await confirm("authorize");
    await page.getByRole("heading", { name: "Historical publication decision receipt", exact: true }).waitFor({ timeout: 30000 });
    s.evidence.push(await snap("14-publication-authorized", true));
  });

  await step(15, "After authorization: review completes, integrator candidate, PR evidence", async (s) => {
    await visit("objectives/" + encodeURIComponent(plan));
    const settled = await waitText(/projection: (?!executing)/, 90000);
    s.notes.push("lifecycle left executing: " + settled);
    s.evidence.push(await snap("15-objective-final", true));
    for (const stage of ["integrate", "review"]) {
      await page.getByLabel("Inspect stage", { exact: true }).selectOption({ label: stage + " · agent" });
      await sleep(500);
      s.evidence.push(await snap("15-stage-" + stage, true));
    }
    await visit("actions");
    s.evidence.push(await snap("15-actions", true));
    await visit("runs");
    s.evidence.push(await snap("15-runs", true));
    const hrefs = await page.locator("main a[href*='runs/']").evaluateAll((a) => [...new Set(a.map((x) => x.getAttribute("href")))]);
    let i = 0;
    for (const href of hrefs) {
      await page.goto(base + "/app" + href);
      await page.getByRole("heading", { level: 1 }).first().waitFor();
      await sleep(900);
      s.evidence.push(await snap("15-run-" + i++, true));
    }
  });

  await step(16, "Integrator candidate: reach publication authority and a PR", async (s) => {
    await visit("objectives/" + encodeURIComponent(plan));
    await page.getByLabel("Inspect stage", { exact: true }).selectOption({ label: "integrate · agent" });
    await sleep(400);
    const href = await page.locator("main a[href*='runs/']").first().getAttribute("href");
    s.notes.push("integrator run link from the objective page: " + href);
    await page.goto(base + "/app" + href);
    await page.getByRole("heading", { level: 1 }).first().waitFor();
    const open = await waitText(/git\.pull_request\.create/, 150000);
    s.evidence.push(await snap("16-integrator-run", true));
    s.notes.push("integrator publication request appeared within 150s: " + open);
    if (!open) {
      s.status = "FAIL";
      return;
    }
    const runId = decodeURIComponent(href.split("runs/")[1]);
    for (const [name, path] of [["run", "/v1/runs/" + encodeURIComponent(runId)], ["requests", "/v1/workspace/requests?limit=50&offset=0&graphs_offset=0"], ["runs", "/v1/runs?limit=50"]]) {
      const response = await fetch(base + path, { headers: { Authorization: "Bearer " + token } });
      await writeFile(resolve(out, "16-api-" + name + ".json"), await response.text());
    }
    await visit("actions");
    const started = Date.now();
    let listed = 0;
    while (Date.now() - started < 150000 && !(listed = await page.locator("[data-request-id]").count())) {
      await sleep(3000);
      await page.reload();
      await page.getByRole("heading", { level: 1 }).first().waitFor();
      await sleep(700);
    }
    s.notes.push("seconds until Action required listed it: " + Math.round((Date.now() - started) / 1000));
    s.evidence.push(await snap("16-actions-while-integrator-awaits", true));
    s.notes.push("Action required cards while the integrator awaits publication authority: " + listed);
    if (listed) {
      await page.locator("[data-request-id]").first().click();
      await sleep(800);
      s.evidence.push(await snap("16-publication-from-actions", true));
    } else {
      s.status = "PARTIAL";
      await page.goto(base + "/app" + href);
      await page.getByRole("heading", { level: 1 }).first().waitFor();
      await sleep(900);
    }
    await page.getByRole("button", { name: "Review & authorize publication", exact: true }).click();
    await page.getByRole("dialog").waitFor();
    await confirm("authorize");
    await page.getByRole("heading", { name: "Historical publication decision receipt", exact: true }).waitFor({ timeout: 30000 });
    s.evidence.push(await snap("16-integrator-authorized", true));
    await page.goto(base + "/app" + href);
    await page.getByRole("heading", { level: 1 }).first().waitFor();
    const pr = await waitText(/Pull request\s*\n?\s*#\d+/, 90000);
    s.evidence.push(await snap("16-integrator-pr", true));
    s.notes.push("integrator PR recorded: " + pr);
    if (!pr) s.status = "FAIL";
  });

  await step(12, "Locate review outcome, assurance, candidate revision, PR and completion", async (s) => {
    await visit("objectives/" + encodeURIComponent(plan));
    await page.getByLabel("Inspect stage", { exact: true }).selectOption({ label: "review · agent" });
    await sleep(500);
    s.evidence.push(await snap("12-review-stage", true));
    const review = (await main()).match(/Review (\w+) · candidate (\w+)/);
    s.notes.push("review outcome on the objective page: " + (review ? review[1] + " @ " + review[2].slice(0, 12) : "not shown"));
    const lifecycle = (await main()).match(/Current controller projection: ([^\n]*)/);
    s.notes.push("objective lifecycle: " + (lifecycle ? lifecycle[1] : "not shown"));
    const objectiveText = await main();
    const prOnObjective = /pull request|#\d+ · open/i.test(objectiveText);
    s.notes.push("PR / publication state shown on the objective page itself: " + prOnObjective + " (otherwise only on the integrator run page, step 16)");
    if (!review) s.status = "FAIL";
    else if (!prOnObjective) s.status = "PARTIAL";
  });

  await step(13, "Agents page: identity vs provider/model vs trust vs eligibility", async (s) => {
    await visit("agents");
    s.evidence.push(await snap("13-agents", true));
    const text = await main();
    const ok = ["AGENT IDENTITY", "PROVIDER / MODEL", "EXECUTION TRUST", "ELIGIBILITY"].every((h) => text.toUpperCase().includes(h));
    s.notes.push("four separate columns: " + ok + "; ineligible agent shown: " + /Not eligible/.test(text));
    if (!ok) s.status = "FAIL";
  });
} finally {
  await writeFile(resolve(out, "rehearsal-results.json"), JSON.stringify({ results, posts, pageErrors }, null, 2) + "\n");
  await context.close();
  await browser.close();
}
