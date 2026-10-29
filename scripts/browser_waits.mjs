// Bounded waits for page state that settles after the render that announces it.
// A route heading can paint before the state-dependent text of the same page,
// and focus and disabled state follow the render, so an assertion that reads
// them once is a race. These wait up to a bound, then fail with the value the
// page actually held: a state that never arrives still fails, only later.
import assert from "node:assert/strict";

// expectText waits for a locator's text to match, then asserts it.
export const expectText = async (locator, pattern, timeout = 10_000) => {
  try {
    await locator.filter({ hasText: pattern }).waitFor({ timeout });
  } catch {
    assert.match(await locator.innerText(), pattern);
  }
};

// eventually polls read() until it equals expected or the bound passes.
export const eventually = async (page, read, expected, what, timeout = 10_000) => {
  const deadline = Date.now() + timeout;
  let actual = await read();
  while (actual !== expected && Date.now() < deadline) {
    await page.waitForTimeout(100);
    actual = await read();
  }
  assert.equal(actual, expected, what);
};
