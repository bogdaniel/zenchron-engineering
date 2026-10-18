import type { Controller } from "./api";
import { ControllerFacts, Disclosure, Head, Link, Panel } from "./components";
import { Facts } from "./runs";

export function ControllerWorkspace({ controller: c, stale }: { controller: Controller; stale: boolean }) {
  return (
    <>
      <Head eyebrow="SYSTEM / CONTROLLER" title="Controller health">
        Identity attestation and live endpoint reachability are independent observations.
      </Head>
      <div className="workspace-grid">
        <div className="workspace-main">
          <Panel title="Controller dimensions">
            <ControllerFacts controller={c} stale={stale} />
          </Panel>
          <Panel title="Diagnostics & troubleshooting">
            <div className="product-body">
              {c.findings.length ? (
                <ul className="diagnostic-list">
                  {c.findings.map((f, i) => (
                    <li key={i}>{f}</li>
                  ))}
                </ul>
              ) : (
                <p>No controller findings reported in this read. That does not qualify every capability as healthy.</p>
              )}
              <p>
                Inspect readiness and configuration with{" "}
                <span className="mono">zenchron-engineering autonomy doctor --text</span>. The check spends no provider
                budget.
              </p>
              <p>For a failed execution, inspect its failure class, held material and successor before any retry.</p>
              <Link to="runs">Inspect runs & recovery</Link>
            </div>
          </Panel>
        </div>
        <aside className="workspace-aside">
          <Panel title="Controller identity records">
            <Disclosure title="Durable and serving build bindings">
              <Facts
                rows={[
                  ["Durable kind", c.durable_generation?.kind || "Not recorded"],
                  ["Durable revision", c.durable_generation?.source_revision || "Not recorded"],
                  ["Durable version", c.durable_generation?.version || "Not recorded"],
                  ["Serving kind", c.live_build?.kind || "Not observed"],
                  ["Serving revision", c.live_build?.source_revision || "Not observed"],
                  ["Serving version", c.live_build?.version || "Not observed"],
                  ["Serving controller binding", c.controller_binding || "Unattested / unobserved"],
                ]}
              />
            </Disclosure>
          </Panel>
        </aside>
      </div>
    </>
  );
}
