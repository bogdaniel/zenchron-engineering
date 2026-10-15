// Real browser, real isolated Go controller/socket/store. Every artifact
// below - the product, the agent profile, the plan template, the objective
// and its approval - is created through the React app's own forms, not
// pre-seeded through Go: this is the operator sequence #471/#479's goal
// names, driven exactly as an operator would drive it.
import assert from "node:assert/strict";
import { readFile, mkdir, writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import { resolve } from "node:path";

const { chromium } = await import(pathToFileURL(process.env.ZENCHRON_PLAYWRIGHT_MODULE).href);
const base = process.env.ZENCHRON_E2E_URL,
  out = process.env.ZENCHRON_E2E_ARTIFACTS || "/private/tmp/zenchron-dashboard-walkthrough";
assert(/^http:\/\/127\.0\.0\.1:\d+$/.test(base));
await mkdir(out, { recursive: true });

const browser = await chromium.launch({
  headless: true,
  executablePath:
    process.env.ZENCHRON_CHROME_EXECUTABLE || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
});
const context = await browser.newContext({ viewport: { width: 1440, height: 1080 }, reducedMotion: "reduce" });
const page = await context.newPage();
const errors = [];
page.on("pageerror", (e) => errors.push(e.message));

const capture = async (name) => page.screenshot({ path: resolve(out, name + ".png"), fullPage: true });
const visit = async (destination) => {
  await page.goto(base + "/app#/" + destination);
  await page.getByRole("heading", { level: 1 }).first().waitFor();
};
const confirm = async (command) => {
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Confirm " + command, exact: true })
    .click();
};
const stage = (index) => page.getByRole("group", { name: `Stage ${index}`, exact: true });

try {
  await page.goto(base + "/app#/products");
  await page
    .getByLabel("Control Plane token", { exact: true })
    .fill((await readFile(process.env.ZENCHRON_E2E_TOKEN_FILE, "utf8")).trim());
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await page.getByRole("heading", { name: "Products", exact: true }).waitFor();
  await capture("01-products-empty");

  // 1. Create a product.
  await page.getByLabel("Name", { exact: true }).fill("walkthrough-product");
  await page.getByLabel("Repositories", { exact: true }).fill("zenchron/seeded");
  await page.getByRole("button", { name: "Review & create", exact: true }).click();
  await page.getByRole("dialog").waitFor();
  assert.match(await page.getByRole("dialog").innerText(), /walkthrough-product/);
  await confirm("product propose");
  await page.getByText(/applied/).first().waitFor();
  await capture("02-product-created");

  // 2. Author an agent profile.
  await visit("agents");
  await page.getByRole("heading", { name: "Create an agent profile", exact: true }).waitFor();
  await page.getByLabel("Profile id", { exact: true }).fill("walkthrough-implementer");
  await page.getByLabel("Execution agent", { exact: true }).selectOption("openai-responses");
  await page.getByLabel("Capabilities", { exact: true }).fill("code_change, repository_analysis, verification");
  await page.getByLabel("Trust requirement", { exact: true }).selectOption("protected");
  await page.getByRole("button", { name: "Review & create", exact: true }).nth(0).click();
  await page.getByRole("dialog").waitFor();
  assert.match(await page.getByRole("dialog").innerText(), /walkthrough-implementer/);
  await confirm("planning artifact propose");
  await page.getByText(/applied/).first().waitFor();
  await capture("03-profile-created");

  // 3. Reload so the profile just written is one the template below can
  // actually offer in its Profile select - a write alone does not change
  // what a form already rendered from workspace.options() can choose from.
  await page.getByRole("button", { name: "Reload registry", exact: true }).click();
  await page.getByRole("dialog").waitFor();
  await confirm("planning reload");
  await page.getByText(/applied/).first().waitFor();
  await capture("03b-registry-reloaded");

  // 4. Author a two-stage plan template, the second depending on the first,
  // the first naming the profile just authored and just reloaded.
  await page.getByLabel("Template id", { exact: true }).fill("walkthrough-feature");
  await stage(1).getByLabel("Stage id", { exact: true }).fill("implementation");
  await stage(1).getByLabel("Profile", { exact: true }).selectOption("walkthrough-implementer");
  await page.getByRole("button", { name: "Add stage", exact: true }).click();
  // "tester", not "reviewer": a reviewer stage needs a structured-verdict
  // capability this fixture's sole configured provider kind cannot produce,
  // which the dashboard correctly refuses to let an operator approve around
  // (planning.tsx: an unbound stage blocks "Review & approve" outright,
  // even though the backend itself would accept the approval - proven
  // directly in cmd/zenchron-engineering/dashboard_acceptance_test.go).
  // "tester" needs only the "verification" capability this walkthrough's
  // own profile already advertises, so the dependency still compiles and
  // binds to a real worker.
  await stage(2).getByLabel("Stage id", { exact: true }).fill("verification");
  await stage(2).getByLabel("Role", { exact: true }).selectOption("tester");
  await stage(2).getByLabel("implementation", { exact: true }).check();
  await page.getByRole("button", { name: "Review & create", exact: true }).nth(1).click();
  await page.getByRole("dialog").waitFor();
  assert.match(await page.getByRole("dialog").innerText(), /walkthrough-feature/);
  await confirm("planning artifact propose");
  await page.getByText(/applied/).first().waitFor();
  await capture("04-template-created");

  // 5. Reload again so the objective below sees the template too.
  await page.getByRole("button", { name: "Reload registry", exact: true }).click();
  await page.getByRole("dialog").waitFor();
  await confirm("planning reload");
  await page.getByText(/applied/).first().waitFor();
  await capture("04b-registry-reloaded");

  // 6. Propose the objective: select the product, which narrows the
  // repository choice to it.
  await visit("intake");
  await page.getByLabel("Product", { exact: true }).selectOption({ label: "walkthrough-product" });
  assert.equal(await page.getByLabel("Repository", { exact: true }).inputValue(), "zenchron/seeded");
  await page.getByLabel("Existing issue", { exact: true }).fill("41");
  await page.getByLabel("Plan template", { exact: true }).selectOption("walkthrough-feature");
  await page.getByLabel("Use deterministic planning", { exact: true }).check();
  await capture("05-intake-filled");
  await page.getByRole("button", { name: "Review objective proposal", exact: true }).click();
  await page.getByRole("dialog").waitFor();
  const proposeResponse = page.waitForResponse((r) => r.url().endsWith("/v1/objectives") && r.request().method() === "POST");
  await confirm("plan revise");
  const proposed = await (await proposeResponse).json();
  assert.equal(proposed.outcome, "applied", JSON.stringify(proposed));
  const planID = proposed.state.plan_id;
  assert(planID, "propose response carried no plan id");
  await capture("06-objective-proposed");

  // 7. Review the compiled plan and approve it.
  await visit(`objectives/${planID}`);
  assert.match(await page.locator("main").innerText(), /implementation/);
  assert.match(await page.locator("main").innerText(), /verification/);
  await capture("07-plan-detail-before-approval");
  await page.getByRole("button", { name: "Review & approve", exact: true }).click();
  await page.getByRole("dialog").waitFor();
  await confirm("plan approve");
  // Approval has its own receipt-based success path (readReceipt), not the
  // plain "applied" announcement the create forms show.
  await page.getByText("Plan approved", { exact: false }).first().waitFor();
  await capture("08-plan-approved");

  // 8. The compiled stage used the authored profile, and the dormant
  // WorkGraph panel is now live.
  const mainText = await page.locator("main").innerText();
  assert.match(mainText, /walkthrough-implementer/, "the authored profile was never bound to its stage");
  assert.match(mainText, /WorkGraph/i, "the WorkGraph panel never lit up for a bound objective");
  await capture("09-workgraph-panel-live");

  assert.deepEqual(errors, []);
  console.log("PASS dashboard walkthrough: product -> profile -> template -> objective -> approval -> WorkGraph");
} catch (error) {
  await capture("failure");
  console.error(error.stack);
  throw error;
} finally {
  await writeFile(resolve(out, "dashboard-walkthrough-errors.json"), JSON.stringify(errors, null, 2) + "\n");
  await context.close();
  await browser.close();
}
