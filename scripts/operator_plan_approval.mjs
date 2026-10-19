// #639 journey driver. Started by TestOperatorPlanApprovalJourney after a plan
// was proposed the way `autonomy plan issue N` proposes it. A real browser
// signs in, finds that plan from Action required without typing an id or a
// digest, reviews it and approves it through the governed UI action. Exits
// non-zero when any step fails; screenshots land in ZENCHRON_E2E_ARTIFACTS.
import { readFile, mkdir, writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import { resolve } from "node:path";
const { chromium } = await import(pathToFileURL(process.env.ZENCHRON_PLAYWRIGHT_MODULE).href);
const base = process.env.ZENCHRON_E2E_URL;
const out = process.env.ZENCHRON_E2E_ARTIFACTS || "/tmp/zc-approval-artifacts";
const token = (await readFile(process.env.ZENCHRON_E2E_TOKEN_FILE, "utf8")).trim();
await mkdir(out, { recursive: true });
const browser = await chromium.launch({
  headless: true,
  executablePath: process.env.ZENCHRON_CHROME_EXECUTABLE || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
});
const page = await (await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: "reduce" })).newPage();
page.on("request", (r) => {
  if (r.method() === "POST") console.log("POST " + new URL(r.url()).pathname);
});
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const main = async () => (await page.locator("main").innerText()).replace(/[ \t]+/g, " ");
const snap = async (name) => {
  const text = await page.locator("body").innerText();
  if (text.includes(token)) throw new Error("token visible on screen in " + name);
  await page.screenshot({ path: resolve(out, name + ".png"), fullPage: true });
  await writeFile(resolve(out, name + ".txt"), (await main()) + "\n");
  console.log(`[${name}] ` + (await main()).slice(0, 900).replace(/\n+/g, " | "));
};
const heading = () => page.getByRole("heading", { level: 1 }).first().waitFor();
const mode = process.env.ZENCHRON_JOURNEY_MODE || "approve";
let failure = "";
try {
  await page.goto(base + "/app#/actions");
  await page.getByLabel("Control Plane token", { exact: true }).fill(token);
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await heading();
  await sleep(1500);
  if (mode === "unobserved") {
    console.log("CONTROL:release-role");
    await sleep(2500);
  }
  // The operator starts where the protocol sends them: the Products page. A
  // plan with no product is not listed there, so the page must say where it is.
  await page.goto(base + "/app#/products");
  await heading();
  await sleep(1200);
  await snap("01-products");
  await page.locator("main").getByRole("link", { name: "Action required", exact: true }).click();
  await heading();
  await sleep(1200);
  await snap("02-action-required");
  // Discover the pending plan from the list: no id, no digest is typed.
  const request = page.locator("a.decision-item").first();
  await request.waitFor({ timeout: 15000 });
  await request.click();
  await sleep(800);
  await snap("03-plan-selected");
  if (mode !== "approve") {
    // The UI must show the controller's real reason and must not bypass it.
    await snap("03b-observed-state");
    const button = page.getByRole("button", { name: "Review & approve", exact: true });
    if (mode === "unobserved") {
      if (!(await button.isDisabled())) throw new Error("approval offered with no controller role");
      if (!/Controller role not held/.test(await main())) throw new Error("blocking condition not shown");
    } else {
      await button.click();
      await page.getByRole("dialog").getByRole("button", { name: "Confirm plan approve", exact: true }).click();
      await sleep(3000);
      await snap("03c-held-refusal");
      const text = await main();
      if (!/configuration_changed/.test(text) || !/propose the issue again/.test(text)) throw new Error("refusal reason not shown: " + text.match(/refused[^\n]*/)?.[0]);
      if (/Historical decision receipt/.test(text)) throw new Error("held plan was approved");
    }
    await browser.close();
    console.log("PASS: " + mode + " state shows its blocking condition");
    process.exit(0);
  }
  const approve = page.getByRole("button", { name: "Review & approve", exact: true });
  await approve.waitFor({ timeout: 15000 });
  if (await approve.isDisabled()) throw new Error("Review & approve is disabled: " + (await main()).match(/[^\n]*(Unavailable|unavailable|blocked)[^\n]*/)?.[0]);
  await approve.click();
  await page.getByRole("dialog").waitFor();
  await snap("04-approve-confirm");
  await page.getByRole("dialog").getByRole("button", { name: "Confirm plan approve", exact: true }).click();
  await page.getByRole("heading", { name: "Historical decision receipt", exact: true }).waitFor({ timeout: 30000 });
  await snap("05-approved");
} catch (error) {
  failure = String(error.message).split("\n")[0];
  try {
    await snap("99-failure");
  } catch {}
} finally {
  await browser.close();
}
if (failure) {
  console.error("FAIL: " + failure);
  process.exit(1);
}
console.log("PASS: CLI-proposed plan approved from the workspace with no typed digest");
