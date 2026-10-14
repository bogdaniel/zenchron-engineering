import { useEffect, useRef, useState } from "react";
import * as api from "./api";
import { TextField } from "./form";

export function Login({
  connected,
  note,
  theme,
  toggleTheme,
}: {
  connected: () => void;
  note?: string;
  theme: string;
  toggleTheme: () => void;
}) {
  const [token, setToken] = useState(""),
    [busy, setBusy] = useState(false),
    [error, setError] = useState("");
  const owner = useRef<AbortController | null>(null),
    input = useRef<HTMLInputElement>(null);
  useEffect(() => {
    input.current?.focus();
    return () => owner.current?.abort();
  }, []);
  const submit = async () => {
    if (busy) return;
    owner.current = new AbortController();
    setBusy(true);
    setError("");
    try {
      await api.connect(token, owner.current.signal);
      setToken("");
      connected();
    } catch (e) {
      if (!owner.current.signal.aborted) {
        const code = e instanceof Error ? e.message : "read_failed";
        setError(
          code === "session_unavailable"
            ? "Browser could not retain the session. Allow cookies for the local Control Plane."
            : code === "unauthorized" || code === "invalid_token"
              ? "Token not accepted. Enter the current Control Plane token."
              : "Connection could not be verified. Check the local endpoint and try again.",
        );
        input.current?.focus();
      }
    } finally {
      if (!owner.current.signal.aborted) setBusy(false);
    }
  };
  return (
    <main id="main" className="login-screen" tabIndex={-1}>
      <section className="panel login-panel" aria-labelledby="login-title">
        <div className="brand">
          <div className="brand-symbol" aria-hidden="true">
            Z<span>+</span>
          </div>
          <div>
            ZENCHRON<small>ENGINEERING OS</small>
          </div>
        </div>
        <div className="product-body">
          <p className="eyebrow">LOCAL CONTROL PLANE</p>
          <h1 id="login-title">Connect to Engineering OS</h1>
          <p>
            Use the token from your local <span className="mono">control-plane.token</span> file. This connects to the
            existing Go listener.
          </p>
          {note && (
            <p className="banner orange" role="status">
              {note}
            </p>
          )}
          <form
            onSubmit={(e) => {
              e.preventDefault();
              void submit();
            }}
          >
            <TextField
              ref={input}
              label="Control Plane token"
              type="password"
              value={token}
              onChange={(e) => setToken(e.target.value)}
              maxLength={64}
              required
              autoComplete="off"
              spellCheck={false}
              disabled={busy}
              help="The token stays out of URLs and local storage. Connecting grants no new controller authority."
              error={error}
            />
            <button className="button primary" disabled={busy}>
              {busy ? "Connecting…" : "Connect"}
            </button>
          </form>
          <button
            className="button login-theme"
            aria-label={`Switch to ${theme === "dark" ? "light" : "dark"} theme`}
            onClick={toggleTheme}
          >
            Change appearance
          </button>
        </div>
      </section>
    </main>
  );
}
