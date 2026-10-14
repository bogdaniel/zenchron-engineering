// Actual Go bearer boundary and public application bootstrap, no HTTP fixtures.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { accessibilityIssues } from "./browser_accessibility.mjs";

export async function checkEngineeringLogin(browser, base, out) {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, reducedMotion: "reduce" });
  const page = await context.newPage(),
    legacyAssets = [],
    writes = [];
  page.on("request", (req) => {
    if (new URL(req.url()).pathname.startsWith("/static/")) legacyAssets.push(true);
    if (req.method() !== "GET") writes.push(true);
  });
  try {
    await page.goto(base + "/app#/runs");
    await page.getByRole("heading", { name: "Connect to Engineering OS", exact: true }).waitFor();
    assert.equal(
      await page.locator("#token").evaluate((n) => n === document.activeElement),
      true,
      "Login starts at token input",
    );
    await page.getByLabel("Control Plane token", { exact: true }).fill("0".repeat(64));
    await page.getByRole("button", { name: "Connect", exact: true }).click();
    await page.getByRole("alert").waitFor();
    assert.match(await page.getByRole("alert").innerText(), /Token not accepted/);
    assert.equal(
      await page.evaluate(() => document.cookie.includes("zenchron_control_plane_token=")),
      false,
      "Refused token never becomes a session",
    );
    let captures = 0;
    for (const theme of ["dark", "light"]) {
      if ((await page.evaluate(() => document.documentElement.dataset.theme)) !== theme)
        await page.getByRole("button", { name: `Switch to ${theme} theme`, exact: true }).click();
      for (const width of [1440, 1920, 390]) {
        await page.setViewportSize({ width, height: width === 390 ? 844 : 1080 });
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
        assert.deepEqual(await accessibilityIssues(page), [], `${theme} login ${width}`);
        await page.screenshot({ path: resolve(out, `login-${width}-${theme}.png`) });
        captures++;
      }
    }
    await page
      .getByLabel("Control Plane token", { exact: true })
      .fill((await readFile(process.env.ZENCHRON_PREVIEW_TOKEN_FILE, "utf8")).trim());
    await page.getByRole("button", { name: "Connect", exact: true }).click();
    await page.getByRole("heading", { name: "Runs & activity", exact: true }).waitFor();
    assert.match(page.url(), /app#\/runs$/, "Login preserves the requested destination");
    for (const destination of ["runs", "objectives"]) {
      await page.goto(base + "/app#/" + destination);
      await page.getByRole("heading", { level: 1 }).waitFor();
      const exact = page.url();
      await page.getByRole("link", { name: "Skip to main content", exact: true }).focus();
      await page.keyboard.press("Enter");
      assert.equal(page.url(), exact, "Keyboard skip preserves the authenticated route");
      assert.equal(
        await page.evaluate(() => document.activeElement?.id),
        "main",
        "Keyboard skip focuses the main landmark",
      );
    }

    assert.equal(await page.evaluate(() => Object.keys(localStorage).some((k) => /token|credential/i.test(k))), false);
    assert.deepEqual(legacyAssets, [], "React login has no old JavaScript/CSS dependency");
    assert.deepEqual(writes, [], "Connecting only authenticates read requests");
    return {
      captures,
      source: "Actual isolated Go authentication boundary; no fixtures",
      checks: [
        "Public code-only shell; wrong token refused without session; current token accepted",
        "Destination preserved; credential absent from local storage; no legacy asset imports",
        "Dark/light login, 1440/1920/390 reflow, keyboard start and readable contrast",
      ],
    };
  } finally {
    await context.close();
  }
}
