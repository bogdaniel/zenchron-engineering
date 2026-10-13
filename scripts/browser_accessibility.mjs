// Focused rendered evidence; full WCAG qualification still requires human audit.
export async function accessibilityIssues(page) {
  return page.evaluate(() => {
    const failures = [],
      parse = (value) => value.match(/[\d.]+/g)?.map(Number) || [];
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
      const style = getComputedStyle(node),
        text = [...node.childNodes]
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
        const a = lum(parse(node instanceof SVGElement ? style.fill : style.color)),
          b = lum(background),
          contrast = (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
        const large =
          parseFloat(style.fontSize) >= 24 ||
          (parseFloat(style.fontSize) >= 18.66 && parseInt(style.fontWeight) >= 700);
        if (contrast < (large ? 3 : 4.5) - 0.01)
          failures.push({ text: text.slice(0, 65), contrast: Number(contrast.toFixed(2)), tag: node.tagName });
      }
      if (node.tagName === "BUTTON" && !node.textContent.trim() && !node.getAttribute("aria-label"))
        failures.push({ name: "Unnamed button" });
    }
    return failures;
  });
}
