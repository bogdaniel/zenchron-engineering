// Real browser, real isolated Go controller/socket/store: an objective is
// described in words, the dashboard opens its GitHub issue (fake forge) after
// a dialog that states the write, and the new issue number flows into the
// same proposal step an existing issue uses.
import assert from "node:assert/strict";
import { readFile, mkdir, writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import { resolve } from "node:path";
import { expectText, eventually } from "./browser_waits.mjs";

const { chromium } = await import(pathToFileURL(process.env.ZENCHRON_PLAYWRIGHT_MODULE).href);
const base = process.env.ZENCHRON_E2E_URL,
  out = process.env.ZENCHRON_E2E_ARTIFACTS || "/private/tmp/zenchron-described-objective";
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
  await page.goto(base + "/app#/intake");
  await page
    .getByLabel("Control Plane token", { exact: true })
    .fill((await readFile(process.env.ZENCHRON_E2E_TOKEN_FILE, "utf8")).trim());
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await page.getByRole("heading", { name: "Create objective", exact: true }).waitFor();

  await page.getByLabel("Describe a new objective instead of using an existing issue").check();
  // A title alone, or a description alone, never offers the GitHub write.
  const review = page.getByRole("button", { name: "Review issue creation", exact: true });
  await page.getByLabel("Objective title", { exact: true }).fill("make the gadget idempotent");
  assert(await review.isDisabled(), "issue creation was offered with a blank description");
  await page.getByLabel("Objective title", { exact: true }).fill("");
  await page.getByLabel("Objective description", { exact: true }).fill("The gadget must be idempotent; prove it with a test.");
  assert(await review.isDisabled(), "issue creation was offered with a blank title");
  await page.getByLabel("Objective title", { exact: true }).fill("make the gadget idempotent");
  await capture("01-described");

  await review.click();
  await page.getByRole("dialog").waitFor();
  await expectText(page.getByRole("dialog"), /writes a new issue to GitHub/);
  const createResponse = page.waitForResponse((r) => r.url().endsWith("/v1/issues") && r.request().method() === "POST");
  await confirm("issue create");
  const created = await (await createResponse).json();
  assert.equal(created.outcome, "applied", JSON.stringify(created));
  await page.getByRole("status").filter({ hasText: /Created/ }).waitFor();
  await eventually(page, () => page.getByLabel("Existing issue", { exact: true }).inputValue(), "42", "the new issue number flows into the proposal step");
  await capture("02-issue-created");

  await page.getByLabel("Use deterministic planning", { exact: true }).check();
  await page.getByRole("button", { name: "Review objective proposal", exact: true }).click();
  await page.getByRole("dialog").waitFor();
  const proposeResponse = page.waitForResponse((r) => r.url().endsWith("/v1/objectives") && r.request().method() === "POST");
  await confirm("plan revise");
  const proposed = await (await proposeResponse).json();
  assert.equal(proposed.outcome, "applied", JSON.stringify(proposed));
  assert(proposed.state.plan_id, "the described objective produced no plan");
  await capture("03-proposed");

  assert.deepEqual(errors, []);
  console.log("PASS described objective: title + description -> GitHub issue -> proposal");
} catch (error) {
  await capture("failure");
  console.error(error.stack);
  throw error;
} finally {
  await writeFile(resolve(out, "described-objective-errors.json"), JSON.stringify(errors, null, 2) + "\n");
  await context.close();
  await browser.close();
}
