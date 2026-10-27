// Actual isolated Go/controller/socket/journal acceptance. No action HTTP mocks.
import assert from "node:assert/strict";
import { readFile, mkdir, writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import { resolve } from "node:path";
import { accessibilityIssues } from "./browser_accessibility.mjs";
const { chromium } = await import(pathToFileURL(process.env.ZENCHRON_PLAYWRIGHT_MODULE).href);
const base = process.env.ZENCHRON_E2E_URL,
  out = process.env.ZENCHRON_E2E_ARTIFACTS || "/private/tmp/zenchron-reconciliation-live";
assert(/^http:\/\/127\.0\.0\.1:\d+$/.test(base));
await mkdir(out, { recursive: true });
const browser = await chromium.launch({
  headless: true,
  executablePath:
    process.env.ZENCHRON_CHROME_EXECUTABLE || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
});
const context = await browser.newContext({ viewport: { width: 1440, height: 1080 }, reducedMotion: "reduce" });
const page = await context.newPage(),
  writes = [],
  errors = [],
  results = [];
page.on("pageerror", (e) => errors.push(e.message));
page.on("request", (r) => {
  if (r.method() === "POST") writes.push({ path: new URL(r.url()).pathname, body: r.postDataJSON() });
});
// The route heading can render before the state-dependent text of the same
// page, so a page-text assertion waits, bounded, for it. A pattern that never
// appears still fails after the timeout, with the text the page actually held.
const expectMain = async (pattern, timeout = 10_000) => {
  try {
    await page.locator("main").filter({ hasText: pattern }).waitFor({ timeout });
  } catch {
    assert.match(await page.locator("main").innerText(), pattern);
  }
};
const check = async (name, run) => {
  await run();
  results.push(name);
  console.log("PASS " + name);
};
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
const capture = async (name) => {
  await page.evaluate(
    () => (document.querySelector(".topbar-product").textContent = "Engineering OS · isolated controller test data"),
  );
  await page.screenshot({ path: resolve(out, name + ".png") });
};
const loseReply = async (pattern) => {
  await page.route(pattern, async (route) => {
    const response = await route.fetch();
    assert.equal((await response.json()).outcome, "applied");
    await route.abort("connectionfailed");
  });
};
const recoverAfterNavigation = async (destination, label, captureName) => {
  await page.getByText(/Outcome unresolved/).waitFor();
  await visit("overview");
  await visit(destination);
  await page.reload();
  await page.getByRole("heading", { name: "Unresolved original operation", exact: true }).waitFor();
  assert.equal(await page.getByRole("button", { name: label, exact: true }).isDisabled(), true);
  if (captureName === "recovered-revision") {
    await page.getByText("Original delivery reference", { exact: true }).click();
    const original = page.getByRole("heading", { name: "Unresolved original operation", exact: true }).locator("..");
    assert.match(await original.innerText(), /revision: 1\b/);
    await expectMain(/plan r2/);
    await page.setViewportSize({ width: 390, height: 844 });
    await capture("unresolved-revision-390-expanded");
    await page.getByText("Original delivery reference", { exact: true }).click();
    await capture("unresolved-revision-390-collapsed");
    await page.setViewportSize({ width: 1440, height: 1080 });
  }
  // A real authenticated read with a wrong exact digest must not resolve it.
  await page.route("**/v1/operator-action-receipts/**", async (route) => {
    const url = new URL(route.request().url());
    url.searchParams.set("digest", "0".repeat(64));
    await route.continue({ url: url.href });
  });
  await page.getByRole("button", { name: "Re-read exact receipt", exact: true }).click();
  await page
    .getByText("Reconciliation unavailable. Outcome remains unknown; no request was resent.", { exact: true })
    .waitFor();
  assert.equal(await page.getByRole("button", { name: label, exact: true }).isDisabled(), true);
  await page.unroute("**/v1/operator-action-receipts/**");
  await page.route("**/v1/operator-action-receipts/**", async (route) => {
    const url = new URL(route.request().url());
    url.pathname = "/v1/operator-action-receipts/00000000-0000-0000-0000-000000000000";
    await route.continue({ url: url.href });
  });
  await page.getByRole("button", { name: "Re-read exact receipt", exact: true }).click();
  await page
    .getByText("No exact delivery receipt observed. Outcome remains unknown; no request was resent.", { exact: true })
    .waitFor();
  await page.unroute("**/v1/operator-action-receipts/**");
  await page.reload();
  await page.getByRole("heading", { name: "Unresolved original operation", exact: true }).waitFor();
  assert.equal(await page.getByRole("button", { name: label, exact: true }).isDisabled(), true);
  await page.getByRole("button", { name: "Re-read exact receipt", exact: true }).click();
  await page.getByRole("heading", { name: "Historical operator action receipt", exact: true }).waitFor();
  assert.equal(await page.locator(".governed-feedback[tabindex]").evaluate((n) => n === document.activeElement), true);
  await capture(captureName);
  await page.getByRole("link", { name: "Inspect recorded result", exact: true }).click();
};
try {
  await check("React login and routing-safe keyboard skip after login/route changes", async () => {
    await page.goto(base + "/app#/objectives");
    await page
      .getByLabel("Control Plane token", { exact: true })
      .fill((await readFile(process.env.ZENCHRON_E2E_TOKEN_FILE, "utf8")).trim());
    await page.getByRole("button", { name: "Connect", exact: true }).click();
    await page.getByRole("heading", { name: "Objectives", exact: true }).waitFor();
    for (const destination of ["objectives", "runs"]) {
      await visit(destination);
      const exact = page.url();
      await page.getByRole("link", { name: "Skip to main content", exact: true }).focus();
      await page.keyboard.press("Enter");
      assert.equal(page.url(), exact);
      assert.equal(await page.evaluate(() => document.activeElement?.id), "main");
    }
  });
  await check("Controller-owned installed agents, profiles and templates", async () => {
    await visit("agents");
    await page.getByRole("region", { name: "Installed agent inventory", exact: true }).waitFor();
    await expectMain(/browser-implementer/);
  });
  await check(
    "Lost objective response survives navigation/reload; exact receipt recovers one unapproved proposal",
    async () => {
      await visit("intake");
      await page.getByLabel("Existing issue", { exact: true }).fill("42");
      await page.getByLabel("Agent profile", { exact: true }).selectOption("browser-implementer");
      await page.getByLabel("Use deterministic planning", { exact: true }).check();
      await page.getByRole("button", { name: "Review objective proposal", exact: true }).click();
      await page.getByRole("dialog").waitFor();
      assert.match(await page.getByRole("dialog").innerText(), /zenchron\/seeded[\s\S]*issue #42\b/);
      await loseReply("**/v1/objectives");
      await page.getByLabel("Decision note (optional)", { exact: true }).fill("PRIVATE_RECOVERY_NOTE");
      await confirm("plan revise");
      await page.getByText(/Outcome unresolved/).waitFor();
      const saved = await page.evaluate(() =>
        Object.keys(localStorage)
          .filter((k) => k.startsWith("zenchron.action-recovery."))
          .map((k) => localStorage.getItem(k))
          .join("\n"),
      );
      assert(!saved.includes("PRIVATE_RECOVERY_NOTE"));
      assert(!saved.includes((await readFile(process.env.ZENCHRON_E2E_TOKEN_FILE, "utf8")).trim()));
      await recoverAfterNavigation("intake", "Review objective proposal", "recovered-objective");
      await page.unroute("**/v1/objectives");
      await page.getByRole("heading", { name: "Plan dependency inspection", exact: true }).waitFor();
      await expectMain(/unapproved/);
      assert.equal(writes.filter((r) => r.path === "/v1/objectives").length, 1);
    },
  );
  await check(
    "Exact assignment refusal uses the real controller; fresh approval records one durable receipt",
    async () => {
      await page.getByRole("button", { name: "Review & approve", exact: true }).click();
      await page.getByRole("dialog").waitFor();
      const binding = await page.getByRole("dialog").innerText();
      assert.match(binding, /exact plan|exact proposed|existing budget|existing policy|budget and policy/i);
      await page.route("**/v1/plans/*/approve", async (route) => {
        const body = route.request().postDataJSON();
        await route.continue({ postData: JSON.stringify({ ...body, assignments_digest: "0".repeat(64) }) });
      });
      await confirm("plan approve");
      await page
        .getByText(/refused/)
        .first()
        .waitFor();
      await page.unroute("**/v1/plans/*/approve");
      await page.getByRole("button", { name: "Review & approve", exact: true }).click();
      await page.getByRole("dialog").waitFor();
      await confirm("plan approve");
      await page.getByRole("heading", { name: "Historical plan approval receipt", exact: true }).waitFor();
      await page
        .locator("main")
        .getByText(/^Plan approved · r1$/)
        .waitFor();
      await expectMain(/Plan approved/);
    },
  );
  await check("Lost revision response retains the original revision across navigation to the latest plan", async () => {
    await visit("objectives/" + encodeURIComponent(process.env.ZENCHRON_E2E_PLAN));
    await page.getByLabel("Use deterministic revision", { exact: true }).check();
    await page.getByRole("button", { name: "Review plan revision", exact: true }).click();
    await page.getByRole("dialog").waitFor();
    await loseReply("**/v1/plans/*/revise");
    await confirm("plan revise");
    await recoverAfterNavigation(
      "objectives/" + encodeURIComponent(process.env.ZENCHRON_E2E_PLAN),
      "Review plan revision",
      "recovered-revision",
    );
    await page.unroute("**/v1/plans/*/revise");
    await page.getByRole("heading", { name: /plan r2/ }).waitFor();
    await expectMain(/unapproved|proposed/);
  });
  await check("Typed graph decision resolves through the controller and retains historical context/focus", async () => {
    await page.setViewportSize({ width: 390, height: 844 });
    await visit("actions");
    await page.locator(`[data-request-id="${process.env.ZENCHRON_E2E_OBJECTIVE_DECISION}"]`).click();
    await page.getByLabel("Requested outcome", { exact: true }).selectOption("allow");
    await page.getByRole("button", { name: "Review decision", exact: true }).click();
    await page.getByRole("dialog").waitFor();
    assert.equal(
      await page
        .getByRole("dialog")
        .locator("details")
        .evaluate((n) => n.open),
      false,
    );
    await capture("decision-confirmation-390-collapsed");
    await page.getByRole("dialog").getByText("Exact subject, evidence & controller", { exact: true }).click();
    assert.match(await page.getByRole("dialog").innerText(), new RegExp(process.env.ZENCHRON_E2E_OBJECTIVE_DECISION));
    const footer = await page
      .getByRole("dialog")
      .getByRole("button", { name: "Confirm decision resolve", exact: true })
      .boundingBox();
    assert(footer.y + footer.height <= 844, "Expanded mobile bindings retain the decision footer");
    await capture("decision-confirmation-390-expanded");
    await confirm("decision resolve");
    await page.getByRole("heading", { name: "Historical decision receipt", exact: true }).waitFor();
    assert.equal(await page.locator(".governed-feedback").evaluate((n) => n === document.activeElement), true);
    await capture("resolved-decision-390-dark");
    await page.setViewportSize({ width: 1440, height: 1080 });
  });
  await check(
    "Publication exact subject, real evidence recording, lost response reconciliation without resend",
    async () => {
      const id = process.env.ZENCHRON_E2E_PUBLICATION_RUN;
      await visit("runs/" + encodeURIComponent(id));
      await page.getByRole("button", { name: "Review & authorize publication", exact: true }).click();
      await page.getByRole("dialog").waitFor();
      assert.match(await page.getByRole("dialog").innerText(), /does not publish or merge/);
      await page.route("**/v1/runs/*/authorize", async (route) => {
        const response = await route.fetch();
        assert.equal((await response.json()).outcome, "applied");
        await route.abort("connectionfailed");
      });
      await confirm("authorize");
      await page.getByText(/Outcome unresolved/).waitFor();
      assert.equal(await page.getByRole("button", { name: "Reject publication", exact: true }).isDisabled(), true);
      await page.unroute("**/v1/runs/*/authorize");
      await page.getByRole("button", { name: "Re-read exact receipt", exact: true }).click();
      await page.getByRole("heading", { name: "Historical publication decision receipt", exact: true }).waitFor();
      assert.equal(writes.filter((r) => r.path.endsWith("/authorize")).length, 1);
      await capture("publication-receipt-1440-dark");
      await page.reload();
      await page
        .locator("p:not(.governed-reason)")
        .getByText("No outstanding human publication request", { exact: true })
        .waitFor();
      assert.equal(
        await page.getByRole("button", { name: "Review & authorize publication", exact: true }).isDisabled(),
        true,
      );
    },
  );
  await check("Pause/unpause use durable run state; resume/refresh refuse unowned input", async () => {
    await visit("runs/browser-run");
    for (const command of ["pause", "unpause", "resume", "refresh"]) {
      await page.getByRole("button", { name: "Review " + command, exact: true }).click();
      await page.getByRole("dialog").waitFor();
      await confirm(command);
      if (["pause", "unpause"].includes(command))
        await page.getByRole("heading", { name: "Observed run control state", exact: true }).last().waitFor();
      else
        await page
          .getByText(/refused/)
          .first()
          .waitFor();
    }
  });
  await check(
    "Lost resume/refresh responses survive navigation and recover exact admission/successor receipts without resend",
    async () => {
      await visit("runs/" + encodeURIComponent(process.env.ZENCHRON_E2E_PUBLICATION_RUN));
      await page.getByRole("button", { name: "Review resume", exact: true }).click();
      await page.getByRole("dialog").waitFor();
      await loseReply("**/v1/runs/*/resume");
      await confirm("resume");
      const destination = "runs/" + encodeURIComponent(process.env.ZENCHRON_E2E_PUBLICATION_RUN);
      await recoverAfterNavigation(destination, "Review resume", "recovered-resume");
      await page.unroute("**/v1/runs/*/resume");
      await page.getByRole("button", { name: "Review refresh", exact: true }).click();
      await page.getByRole("dialog").waitFor();
      const prior = page.url();
      await loseReply("**/v1/runs/*/refresh");
      await confirm("refresh");
      await recoverAfterNavigation(destination, "Review refresh", "recovered-refresh");
      await page.unroute("**/v1/runs/*/refresh");
      await page.waitForURL((url) => url.href !== prior);
      assert.match(page.url(), /#\/runs\//);
      await page.getByRole("heading", { level: 1 }).waitFor();
      assert.equal(
        writes.filter((r) => r.path === `/v1/runs/${process.env.ZENCHRON_E2E_PUBLICATION_RUN}/resume`).length,
        1,
      );
      assert.equal(
        writes.filter((r) => r.path === `/v1/runs/${process.env.ZENCHRON_E2E_PUBLICATION_RUN}/refresh`).length,
        1,
      );
      assert.equal(writes.filter((r) => r.path.endsWith("/revise")).length, 1);
    },
  );
  await check("Unavailable browser recovery storage refuses before sending any command", async () => {
    await visit("intake");
    await page.getByLabel("Existing issue", { exact: true }).fill("43");
    await page.getByLabel("Use deterministic planning", { exact: true }).check();
    await page.evaluate(() => {
      window.fixtureRecoverySetItem = Storage.prototype.setItem;
      Storage.prototype.setItem = function (key, value) {
        if (key.startsWith("zenchron.action-recovery."))
          throw new DOMException("Isolated storage refusal", "QuotaExceededError");
        return window.fixtureRecoverySetItem.call(this, key, value);
      };
    });
    const count = writes.length;
    await page.getByRole("button", { name: "Review objective proposal", exact: true }).click();
    await page.getByRole("dialog").waitFor();
    await confirm("plan revise");
    await page.getByText("Recovery reference could not be saved. Nothing was sent.", { exact: true }).waitFor();
    assert.equal(writes.length, count);
    await page.evaluate(() => {
      Storage.prototype.setItem = window.fixtureRecoverySetItem;
      delete window.fixtureRecoverySetItem;
    });
  });
  await check("Dark/light responsive and accessible actual application compositions", async () => {
    for (const theme of ["dark", "light"])
      for (const width of [1440, 1920, 390])
        for (const [name, destination] of [
          ["overview", "overview"],
          ["portfolio", "objectives"],
          ["workspace", "objectives/" + process.env.ZENCHRON_E2E_PLAN],
          ["actions", "actions"],
          ["run", "runs/browser-run"],
          ["graph", "workgraphs/" + process.env.ZENCHRON_E2E_OBJECTIVE_GRAPH],
          ["intake", "intake"],
        ]) {
          await page.setViewportSize({ width, height: width === 390 ? 844 : 1080 });
          await visit(destination);
          if ((await page.evaluate(() => document.documentElement.dataset.theme)) !== theme)
            await page.getByRole("button", { name: `Switch to ${theme} theme`, exact: true }).click();
          assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
          assert.deepEqual(await accessibilityIssues(page), [], `${name} ${width} ${theme}`);
          if (width === 390 && theme === "dark" && ["actions", "workspace"].includes(name)) {
            const session = await context.newCDPSession(page);
            const tree = await session.send("Accessibility.getFullAXTree");
            await writeFile(
              resolve(out, `${name}-390-accessibility-tree.json`),
              JSON.stringify(
                {
                  source: "Chromium accessibility tree; manual screen-reader speech remains unqualified",
                  nodes: tree.nodes.map((n) => ({ role: n.role?.value, name: n.name?.value, ignored: n.ignored })),
                },
                null,
                2,
              ) + "\n",
            );
            await session.detach();
          }
          await capture(`${name}-${width}-${theme}`);
        }
  });
  assert.deepEqual(errors, []);
  console.log(
    JSON.stringify({
      checks: results,
      posts: writes.map((w) => w.path),
      source: "Actual isolated controller/state; no production authorization, publication or acceptance",
    }),
  );
} catch (error) {
  await capture("failure");
  console.error(error.stack);
  throw error;
} finally {
  await writeFile(
    resolve(out, "react-browser-results.json"),
    JSON.stringify({ checks: results, posts: writes.map((w) => w.path), errors }, null, 2) + "\n",
  );
  await context.close();
  await browser.close();
}
