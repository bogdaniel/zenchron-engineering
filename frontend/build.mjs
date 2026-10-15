import { build } from "esbuild";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { readFile, writeFile } from "node:fs/promises";

await build({
  entryPoints: ["src/main.tsx"],
  bundle: true,
  minify: true,
  outfile: "../controlplane/assets/design/react.js",
  platform: "browser",
  target: "es2022",
  jsx: "automatic",
  define: { "process.env.NODE_ENV": '"production"' },
  banner: { js: "// Generated, do not edit. Run npm ci && npm run build in frontend/." },
  legalComments: "inline",
});

// esbuild never touches CSS; these four files are Tailwind-authored source
// (frontend/src/*.css) compiled 1:1 to the same output filenames the Go
// server already embeds and serves - no Go-side change needed.
const run = promisify(execFile);
const cssBanner = "/* Generated, do not edit. Run npm ci && npm run build in frontend/. */\n";
for (const name of ["tokens", "layout", "components", "runtime"]) {
  const outfile = `../controlplane/assets/design/${name}.css`;
  // --minify alone drops any custom property it can't prove is used WITHIN
  // this file - tokens.css declares most of its properties only for the
  // other three files to consume via var(...), which this pass can't see.
  // Pairing it with --optimize keeps those (verified: --minify alone strips
  // --bg/--sidebar/etc.; --optimize --minify together keeps them, still
  // minified) while still producing fully minified output.
  await run("node_modules/.bin/tailwindcss", ["-i", `src/${name}.css`, "-o", outfile, "--optimize", "--minify"]);
  await writeFile(outfile, cssBanner + (await readFile(outfile, "utf8")));
}
