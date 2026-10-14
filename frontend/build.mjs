import { build } from "esbuild";

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
