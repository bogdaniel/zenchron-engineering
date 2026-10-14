// Two real, concurrently running Control Plane instances (TestBrowserCrossInstanceAuthentication):
// separate state directories, tokens and loopback ports, one browser, one cookie jar. Proves
// sessionCookieName's port scoping (controlplane/web.go, frontend/src/api.ts): logging into one
// instance must never evict or shadow the other's session, and an expired/invalid credential must
// recover cleanly through Login rather than a broken authenticated-looking shell.
import assert from "node:assert/strict";
import { mkdir, writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import { resolve } from "node:path";
const { chromium } = await import(pathToFileURL(process.env.ZENCHRON_PLAYWRIGHT_MODULE).href);

const urlA = process.env.ZENCHRON_E2E_URL_A,
  tokenA = process.env.ZENCHRON_E2E_TOKEN_A,
  urlB = process.env.ZENCHRON_E2E_URL_B,
  tokenB = process.env.ZENCHRON_E2E_TOKEN_B;
assert(/^http:\/\/127\.0\.0\.1:\d+$/.test(urlA) && /^http:\/\/127\.0\.0\.1:\d+$/.test(urlB));
assert(new URL(urlA).port !== new URL(urlB).port, "The two instances must be on different ports");
const out = process.env.ZENCHRON_E2E_ARTIFACTS || "/private/tmp/zenchron-cross-instance-live";
await mkdir(out, { recursive: true });

const browser = await chromium.launch({
  headless: true,
  executablePath:
    process.env.ZENCHRON_CHROME_EXECUTABLE || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
});
// One context: one cookie jar, exactly what a single browser gives an operator
// signed in to two instances on the same hostname but different ports.
const context = await browser.newContext({ viewport: { width: 1440, height: 1080 }, reducedMotion: "reduce" });
const errors = [],
  results = [];
context.on("page", (p) => p.on("pageerror", (e) => errors.push(e.message)));
const check = async (name, run) => {
  await run();
  results.push(name);
  console.log("PASS " + name);
};
const login = async (page, base, token) => {
  await page.goto(base + "/app#/runs");
  await page.getByLabel("Control Plane token", { exact: true }).fill(token);
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await page.getByRole("heading", { name: "Runs & activity", exact: true }).waitFor();
};
const capture = async (page, name) => {
  await page.screenshot({ path: resolve(out, name + ".png") });
};

let pageA, pageB;
try {
  await check("Login to instance A shows its own real seeded run, not instance B's", async () => {
    pageA = await context.newPage();
    await login(pageA, urlA, tokenA);
    assert.match(await pageA.locator("main").innerText(), /instance-a-run/);
    assert.doesNotMatch(await pageA.locator("main").innerText(), /instance-b-run/);
    await capture(pageA, "instance-a-authenticated-runs");
  });
  await check("Controller Health on instance A reflects a real, unreachable live observation", async () => {
    await pageA.goto(urlA + "/app#/controller");
    await pageA.getByRole("heading", { name: "Controller health", exact: true }).waitFor();
    assert.match(await pageA.locator("main").innerText(), /Unreachable/);
    await capture(pageA, "instance-a-controller-health");
  });
  await check("Visiting instance B in the same browser starts at Login, not a borrowed session", async () => {
    pageB = await context.newPage();
    await pageB.goto(urlB + "/app#/runs");
    await pageB.getByRole("heading", { name: "Connect to Engineering OS", exact: true }).waitFor();
  });
  await check("Login to instance B shows its own real seeded run, isolated from instance A", async () => {
    await login(pageB, urlB, tokenB);
    assert.match(await pageB.locator("main").innerText(), /instance-b-run/);
    assert.doesNotMatch(await pageB.locator("main").innerText(), /instance-a-run/);
    await capture(pageB, "instance-b-authenticated-runs");
  });
  await check("Switching back to instance A: still authenticated, own data unchanged", async () => {
    await pageA.bringToFront();
    // A fresh navigation/reload of instance A's own runs view, not a mere
    // tab focus: the session must survive a real page load, not only the
    // in-memory React state of a tab that was never reloaded.
    await pageA.goto(urlA + "/app#/runs");
    await pageA.getByRole("heading", { name: "Runs & activity", exact: true }).waitFor();
    assert.match(await pageA.locator("main").innerText(), /instance-a-run/);
  });
  await check("An invalid token at the login form is refused without starting a session", async () => {
    await pageB.evaluate(() => {
      document.cookie.split(";").forEach((c) => {
        const name = c.split("=")[0].trim();
        if (name.startsWith("zenchron_control_plane_token_"))
          document.cookie = name + "=; path=/; SameSite=Strict; Max-Age=0";
      });
    });
    await pageB.reload();
    await pageB.getByRole("heading", { name: "Connect to Engineering OS", exact: true }).waitFor();
    await pageB.getByLabel("Control Plane token", { exact: true }).fill("0".repeat(64));
    await pageB.getByRole("button", { name: "Connect", exact: true }).click();
    await pageB.getByRole("alert").waitFor();
    assert.match(await pageB.getByRole("alert").innerText(), /Token not accepted/);
  });
  await check("Expired/invalid credential recovers cleanly through Login, not a broken shell", async () => {
    // Simulate instance A's token being rotated/invalidated server-side
    // while the browser still holds the old, syntactically valid cookie.
    await pageA.evaluate(() => {
      const port = location.port || "default";
      document.cookie = `zenchron_control_plane_token_${port}=` + "f".repeat(64) + "; path=/; SameSite=Strict";
    });
    await pageA.getByRole("button", { name: "Refresh", exact: true }).click();
    await pageA.getByRole("heading", { name: "Connect to Engineering OS", exact: true }).waitFor();
    assert.match(
      await pageA.locator("main").innerText(),
      /Your session ended\. Sign in again to continue\./,
      "A distinct recovery message, not the generic read-failure panel",
    );
    await capture(pageA, "instance-a-session-ended-recovery");
    await login(pageA, urlA, tokenA);
    assert.match(await pageA.locator("main").innerText(), /instance-a-run/);
  });
  await check("Instance B recovers with its correct token, unaffected by instance A's session changes", async () => {
    await pageB.bringToFront();
    await pageB.reload();
    await pageB.getByLabel("Control Plane token", { exact: true }).fill(tokenB);
    await pageB.getByRole("button", { name: "Connect", exact: true }).click();
    await pageB.getByRole("heading", { name: "Runs & activity", exact: true }).waitFor();
    assert.match(await pageB.locator("main").innerText(), /instance-b-run/);
  });
  assert.deepEqual(errors, []);
  console.log(
    JSON.stringify({
      checks: results,
      source: "Two real, concurrently running Control Plane instances; isolated SQLite state, no design fixtures",
    }),
  );
} catch (error) {
  if (pageA) await capture(pageA, "failure-a");
  if (pageB) await capture(pageB, "failure-b");
  console.error(error.stack);
  throw error;
} finally {
  await writeFile(
    resolve(out, "cross-instance-auth-results.json"),
    JSON.stringify({ checks: results, errors }, null, 2) + "\n",
  );
  await context.close();
  await browser.close();
}
