import { ObjectiveIntake } from "./planning";
import { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import * as api from "./api";
import { Empty, Head, Link, Panel } from "./components";
import { Overview, Portfolio, Workspace } from "./pages";
import { Decisions } from "./decisions";
import { Login } from "./login";
import { ControllerWorkspace } from "./system";
import { InstalledAgents } from "./installed-agents";
import { JournalHistory } from "./history";
import * as ops from "./operations-api";
import { RunPortfolio, RunWorkspace } from "./runs";
import { GraphPortfolio, GraphWorkspace, ProductPortfolio, ProductWorkspace } from "./workgraphs";

const navigation = [
  ["overview", "Engineering overview", "▦"],
  ["objectives", "Objectives", "≡"],
  ["actions", "Action required", "◇"],
  ["runs", "Runs & activity", "▷"],
  ["workgraphs", "WorkGraphs", "⌘"],
  ["products", "Products", "□"],
  ["agents", "Agents", "∘"],
  ["controller", "Controller health", "⌁"],
] as const;
type Route = { page: string; id: string; revision: number; offset: number };
type Snapshot = {
  controller?: api.Controller;
  index?: api.PlanIndex;
  runs?: api.Runs;
  context?: api.Context;
  run?: ops.RunDetail;
  graphs?: ops.Graphs;
  graph?: ops.GraphDetail;
  products?: ops.Products;
  product?: ops.ProductDetail;
  fleet?: ops.Fleet;
};
function route(): Route {
  try {
    const [path, search = ""] = location.hash.replace(/^#\/?/, "").split("?");
    const query = new URLSearchParams(search),
      offset = Number(query.get("offset") || 0);
    if (
      [...query.keys()].some((key) => key !== "offset") ||
      query.getAll("offset").length > 1 ||
      !Number.isSafeInteger(offset) ||
      offset < 0 ||
      offset > 2147483647
    )
      throw new Error();
    const parts = path.split("/");
    const revision = parts[2] ? Number(parts[2]) : 0;
    if (!Number.isSafeInteger(revision) || revision < 0 || parts.length > 3) throw new Error();
    return { page: parts[0] || "overview", id: decodeURIComponent(parts[1] || ""), revision, offset };
  } catch {
    return { page: "invalid", id: "", revision: 0, offset: 0 };
  }
}
// Read failures are not one condition. An expired/invalid credential ends
// the session (Screen hands that to onSessionInvalid instead); these cover
// what's left - the controller, the local database and the runtime refusing
// an otherwise-authenticated read - each with its own recovery, never the
// same generic sentence.
const readFailureCopy: Record<string, { title: string; detail: string }> = {
  controller_unreachable: {
    title: "Controller unreachable",
    detail: "The execution controller could not be reached. Durable plans and runs may still be readable.",
  },
  invalid_controller_response: {
    title: "Controller response invalid",
    detail: "The execution controller replied, but its response could not be trusted as a snapshot.",
  },
  read_failed: {
    title: "Database read failed",
    detail: "The local durable store could not be read. No unknown fact is interpreted as success.",
  },
  permission_refused: {
    title: "Action not permitted",
    detail: "This credential does not permit this read.",
  },
};
function Screen({ route: r, onSessionInvalid }: { route: Route; onSessionInvalid: () => void }) {
  const [data, setData] = useState<Snapshot | null>(null);
  const [loading, setLoading] = useState(true),
    [error, setError] = useState("");
  const [receivedAt, setReceivedAt] = useState(""),
    [stale, setStale] = useState(false);
  const [offset, setOffset] = useState(r.offset),
    [query, setQuery] = useState(""),
    [filter, setFilter] = useState("all");
  const [refreshKey, setRefreshKey] = useState(0);
  useEffect(() => {
    const timer = setInterval(() => {
      // Pause observation while hidden or confirming an immutable action.
      if (document.visibilityState === "visible" && !document.querySelector("dialog[open]"))
        setRefreshKey((k) => k + 1);
    }, 20000);
    return () => clearInterval(timer);
  }, []);
  useEffect(() => {
    const owner = new AbortController();
    let expiry: ReturnType<typeof setTimeout> | undefined;
    setLoading(true);
    setError("");
    setStale(true);
    Promise.all([
      ["overview", "actions", "controller"].includes(r.page) ||
      (r.page === "objectives" && !!r.id) ||
      (r.page === "workgraphs" && !!r.id) ||
      (r.page === "runs" && !!r.id) ||
      (r.page === "products" && !r.id)
        ? api.controller(owner.signal)
        : Promise.resolve(undefined),
      ["overview", "actions"].includes(r.page) || (r.page === "objectives" && !r.id)
        ? api.plans(owner.signal, offset, query)
        : Promise.resolve(undefined),
      r.page === "overview" || (r.page === "runs" && !r.id)
        ? api.runs(owner.signal, offset, filter === "all" ? "" : filter, query)
        : Promise.resolve(undefined),
      r.id && ["objectives", "actions"].includes(r.page)
        ? api.context(r.id, owner.signal, r.revision)
        : Promise.resolve(undefined),
      r.page === "runs" && r.id ? ops.run(r.id, owner.signal) : Promise.resolve(undefined),
      r.page === "workgraphs" && !r.id ? ops.graphs(offset, owner.signal) : Promise.resolve(undefined),
      r.page === "workgraphs" && r.id ? ops.graph(r.id, owner.signal) : Promise.resolve(undefined),
      r.page === "products" && !r.id ? ops.products(offset, owner.signal) : Promise.resolve(undefined),
      r.page === "products" && r.id ? ops.product(r.id, offset, owner.signal) : Promise.resolve(undefined),
      r.page === "overview" ? ops.fleet(owner.signal) : Promise.resolve(undefined),
    ])
      .then(([controller, index, runs, context, run, graphs, graph, products, product, fleet]) => {
        if (owner.signal.aborted) return;
        setData({
          controller,
          index,
          runs,
          context,
          run,
          graphs,
          graph: context?.graph ? ops.decodeGraph(context.graph) : graph,
          products,
          product,
          fleet,
        });
        setReceivedAt(new Date().toISOString());
        setStale(false);
        // UI observation age only; no liveness or authority lease is inferred.
        expiry = setTimeout(() => setStale(true), 30000);
      })
      .catch((e) => {
        if (owner.signal.aborted) return;
        const code = e instanceof Error ? e.message : "read_failed";
        // An expired or invalid credential is not one more unreadable view
        // among others: nothing behind this chrome is authenticated any
        // more, so the whole session ends here rather than leaving a broken
        // authenticated-looking shell on screen.
        if (code === "unauthorized") {
          onSessionInvalid();
          return;
        }
        setError(code);
        setStale(true);
      })
      .finally(() => {
        if (!owner.signal.aborted) setLoading(false);
      });
    return () => {
      owner.abort();
      if (expiry) clearTimeout(expiry);
    };
  }, [r.page, r.id, r.revision, offset, query, filter, refreshKey]);
  const refresh = () => setRefreshKey((k) => k + 1);
  let content;
  if (!data)
    content = loading ? (
      <section className="panel loading-state" aria-busy="true">
        <div className="skeleton wide" />
        <div className="skeleton tall" />
        <p role="status">Reading runtime state…</p>
      </section>
    ) : (
      <Panel title={readFailureCopy[error]?.title || "Engineering state unavailable"}>
        <Empty title="This view could not be read">
          {readFailureCopy[error]?.detail || `${error}. No unknown fact is interpreted as success.`}
        </Empty>
        <div className="panel-footer">
          <button className="button" onClick={refresh}>
            Retry read
          </button>
        </div>
      </Panel>
    );
  else if (r.page === "overview" && data.runs && data.controller && data.index)
    content = (
      <Overview
        controller={data.controller}
        index={data.index}
        runs={data.runs}
        stale={stale || loading}
        fleet={data.fleet}
      />
    );
  else if (r.page === "objectives" && !r.id && data.index)
    content = (
      <Portfolio
        index={data.index}
        query={query}
        filter={filter}
        onFilter={setFilter}
        onQuery={(q) => {
          setQuery(q);
          setOffset(0);
        }}
        onPage={setOffset}
      />
    );
  else if (r.page === "objectives" && data.context)
    content = (
      <Workspace
        key={`${r.id}/${data.context.plan.revision}`}
        context={data.context}
        graph={data.graph}
        controller={data.controller}
        stale={stale || loading}
        refresh={refresh}
      />
    );
  else if (r.page === "runs" && !r.id && data.runs)
    content = (
      <RunPortfolio
        runs={data.runs}
        onPage={setOffset}
        status={filter}
        source={query}
        onStatus={(v) => {
          setFilter(v);
          setOffset(0);
        }}
        onSource={(v) => {
          setQuery(v);
          setOffset(0);
        }}
      />
    );
  else if (r.page === "runs" && data.run && data.controller)
    content = <RunWorkspace run={data.run} controller={data.controller} stale={stale || loading} after={r.revision} />;
  else if (r.page === "workgraphs" && data.graphs) content = <GraphPortfolio graphs={data.graphs} onPage={setOffset} />;
  else if (r.page === "workgraphs" && data.graph)
    content = (
      <GraphWorkspace
        key={`${r.id}/${data.graph.revision}`}
        graph={data.graph}
        controller={data.controller}
        stale={stale || loading}
      />
    );
  else if (r.page === "products" && data.products)
    content = (
      <ProductPortfolio
        products={data.products}
        onPage={setOffset}
        controller={data.controller}
        stale={stale || loading}
        onSettled={refresh}
      />
    );
  else if (r.page === "products" && data.product)
    content = <ProductWorkspace product={data.product} onPage={setOffset} />;
  else if (r.page === "intake") content = <ObjectiveIntake />;
  else if (r.page === "agents") content = <InstalledAgents />;
  else if (r.page === "attempts" && r.id)
    content = (
      <>
        <Head eyebrow="PLANNING / ATTEMPT HISTORY" title="Planning attempt history">
          A refused attempt has no compiled or executable plan revision. Journal metadata remains inspectable.
        </Head>
        <Panel title="Source identity">
          <div className="product-body">
            <p className="mono exact-value">{r.id}</p>
            <p>
              Inspect typed refusal detail through <span className="mono">autonomy plan show</span>. Private reasoning
              and payloads stay outside the read API.
            </p>
            <Link to="objectives">Back to objectives</Link>
          </div>
        </Panel>
        <JournalHistory kind="plan" id={r.id} />
      </>
    );
  else if (r.page === "actions" && data.index && data.controller)
    content = (
      <Decisions
        index={data.index}
        context={data.context}
        controller={data.controller}
        stale={stale || loading}
        refresh={refresh}
        onPage={setOffset}
      />
    );
  else if (r.page === "controller" && data.controller)
    content = <ControllerWorkspace controller={data.controller} stale={stale || loading} />;
  else
    content = (
      <Panel title="Destination unavailable">
        <Empty title="No supported destination">
          Use Overview, Objectives or Action required. No subject is inferred from an invalid route.
        </Empty>
      </Panel>
    );
  return (
    <>
      <div className="preview-bar">
        <div>
          <span className="fixture-label">RUNTIME</span>
          <span>Observed API data</span>
        </div>
        <button className="button" disabled={loading} onClick={refresh}>
          {loading ? "Reading…" : "Refresh"}
        </button>
      </div>
      <main id="main" tabIndex={-1}>
        {data && (stale || error || loading) && (
          <div className="banner orange connection-banner" role="status">
            <div>
              <strong>
                {error
                  ? `Read unavailable · ${error}`
                  : loading
                    ? "Refreshing · decisions unavailable"
                    : "Snapshot stale · decisions unavailable"}
              </strong>
              <p>Retained observations are historical. Refresh the exact subject before acting.</p>
            </div>
          </div>
        )}
        {content}
      </main>
      <footer className="app-footer">
        <span>{receivedAt ? `Browser received ${receivedAt}` : "No snapshot received"}</span>
        <span>Receipt time does not establish live reachability.</span>
      </footer>
    </>
  );
}
function App() {
  const [connected, setConnected] = useState(api.hasCredential);
  const [sessionNote, setSessionNote] = useState("");
  const [current, setCurrent] = useState(route);
  const [theme, setTheme] = useState(() => (localStorage.getItem("zenchron-theme") === "light" ? "light" : "dark"));
  const [menu, setMenu] = useState(false);
  const [mobile, setMobile] = useState(() => matchMedia("(max-width: 760px)").matches);
  const sidebar = useRef<HTMLElement>(null),
    toggle = useRef<HTMLButtonElement>(null);
  const menuOpener = useRef<HTMLButtonElement | null>(null);
  useEffect(() => {
    const media = matchMedia("(max-width: 760px)");
    const changed = () => {
      setMobile(media.matches);
      if (!media.matches) setMenu(false);
    };
    media.addEventListener("change", changed);
    return () => media.removeEventListener("change", changed);
  }, []);
  useEffect(() => {
    const link = document.querySelector<HTMLAnchorElement>(".skip-link");
    const skip = (event: MouseEvent) => {
      const main = document.getElementById("main");
      if (!main) return;
      event.preventDefault();
      main.focus();
      main.scrollIntoView({ block: "start" });
    };
    link?.addEventListener("click", skip);
    return () => link?.removeEventListener("click", skip);
  }, []);
  const openMenu = (opener: HTMLButtonElement) => {
    menuOpener.current = opener;
    setMenu(true);
  };
  const closeMenu = () => {
    setMenu(false);
    requestAnimationFrame(() => menuOpener.current?.focus());
  };
  useEffect(() => {
    const changed = () => {
      setCurrent(route());
      setMenu(false);
      requestAnimationFrame(() => {
        window.scrollTo(0, 0);
        document.getElementById("main")?.focus({ preventScroll: true });
      });
    };
    addEventListener("hashchange", changed);
    return () => removeEventListener("hashchange", changed);
  }, []);
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    localStorage.setItem("zenchron-theme", theme);
  }, [theme]);
  useEffect(() => {
    if (!menu) return;
    const container = sidebar.current;
    const controls = () => [...(container?.querySelectorAll<HTMLElement>("a,button") || [])];
    controls()[0]?.focus();
    const key = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        closeMenu();
      }
      if (e.key === "Tab") {
        const items = controls(),
          first = items[0],
          last = items.at(-1);
        if (e.shiftKey && document.activeElement === first) {
          e.preventDefault();
          last?.focus();
        }
        if (!e.shiftKey && document.activeElement === last) {
          e.preventDefault();
          first?.focus();
        }
      }
    };
    document.querySelector(".application")?.setAttribute("inert", "");
    document.addEventListener("keydown", key);
    return () => {
      document.removeEventListener("keydown", key);
      document.querySelector(".application")?.removeAttribute("inert");
    };
  }, [menu]);
  if (!connected)
    return (
      <Login
        connected={() => {
          setSessionNote("");
          setConnected(true);
        }}
        note={sessionNote}
        theme={theme}
        toggleTheme={() => setTheme(theme === "dark" ? "light" : "dark")}
      />
    );
  return (
    <>
      <aside
        ref={sidebar}
        className={`sidebar runtime-sidebar ${menu ? "is-open" : ""}`}
        aria-label="Engineering OS navigation"
        inert={mobile && !menu}
        role={menu ? "dialog" : undefined}
        aria-modal={menu ? true : undefined}
      >
        <Link to="overview" className="brand">
          <div className="brand-symbol" aria-hidden="true">
            Z<span>+</span>
          </div>
          <div>
            ZENCHRON<small>ENGINEERING OS</small>
          </div>
        </Link>
        <div className="local-workspace">
          <div className="workspace-mark">Z</div>
          <div>
            <strong>Local control plane</strong>
            <small>Governed runtime</small>
          </div>
        </div>
        <div className="nav-label eyebrow">OPERATIONS</div>
        <nav>
          {navigation.map(([to, label, icon]) => (
            <a key={to} href={`#/${to}`} aria-current={current.page === to ? "page" : undefined}>
              <span aria-hidden="true">{icon}</span>
              {label}
            </a>
          ))}
        </nav>
        <div className="sidebar-bottom">
          <p className="sidebar-health">Identity records and endpoint reachability are inspected separately.</p>
          <div className="sidebar-edition">
            <span>LOCAL CONTROL PLANE</span>
            <span>Existing governed backend</span>
          </div>
        </div>
        {menu && (
          <button className="button" onClick={closeMenu}>
            Close navigation
          </button>
        )}
      </aside>
      <div className="application">
        <header className="topbar">
          <div className="topbar-location">
            <button
              ref={toggle}
              className="icon-button menu-toggle"
              aria-label="Open navigation"
              aria-expanded={menu}
              onClick={(event) => openMenu(event.currentTarget)}
            >
              ☰
            </button>
            <span className="topbar-product">Engineering OS</span>
            <span className="topbar-slash">/</span>
            <span className="muted">Local workspace</span>
          </div>
          <div className="topbar-tools">
            <button
              className="icon-button"
              aria-label={`Switch to ${theme === "dark" ? "light" : "dark"} theme`}
              onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
            >
              {theme === "dark" ? "☀" : "◐"}
            </button>
          </div>
        </header>
        <Screen
          key={`${current.page}/${current.id}/${current.revision}`}
          route={current}
          onSessionInvalid={() => {
            api.disconnect();
            setSessionNote("Your session ended. Sign in again to continue.");
            setConnected(false);
          }}
        />
        <nav className="mobile-nav" aria-label="Mobile navigation">
          {navigation
            .filter(([to]) => ["overview", "objectives", "actions"].includes(to))
            .map(([to, label, icon]) => (
              <a key={to} href={`#/${to}`} aria-current={current.page === to ? "page" : undefined}>
                <span aria-hidden="true">{icon}</span>
                <span>{to === "overview" ? "Overview" : to === "actions" ? "Decisions" : label}</span>
              </a>
            ))}
          <button aria-label="More navigation" onClick={(event) => openMenu(event.currentTarget)}>
            <span aria-hidden="true">☰</span>
            <span>More</span>
          </button>
        </nav>
      </div>
    </>
  );
}
const root = document.getElementById("app");
if (!root) throw new Error("Application root missing");
document.title = "Zenchron · Engineering OS";
createRoot(root).render(<App />);
