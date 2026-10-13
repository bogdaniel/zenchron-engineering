import type { Controller, Runs } from "./api";
import { Badge, ControllerFacts, Disclosure, Empty, Head, Link, Pagination, Panel } from "./components";
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
export function Agents({ runs, onPage }: { runs: Runs; onPage: (v: number) => void }) {
  return (
    <>
      <Head eyebrow="SYSTEM / EXECUTION AGENTS" title="Agents">
        Recorded execution bindings in the loaded run page. Agent identity, provider kind and execution trust remain
        separate.
      </Head>
      <Panel title="Observed agent bindings" extra={<Badge>{runs.runs.length} run observations</Badge>}>
        {runs.runs.length ? (
          <div className="table-scroll" tabIndex={0} role="region" aria-label="Observed agent bindings">
            <table className="data-table">
              <thead>
                <tr>
                  {["Agent identity", "Provider / model", "Execution trust", "Source run"].map((s) => (
                    <th scope="col" key={s}>
                      {s}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {runs.runs.map((r) => (
                  <tr key={r.run_id}>
                    <td data-label="Agent identity">{r.agent || "Not recorded"}</td>
                    <td data-label="Provider / model">
                      {r.provider_kind || "Unknown"} / {r.model || "Provider default not recorded"}
                    </td>
                    <td data-label="Execution trust">{r.trust_mode || "Unknown"}</td>
                    <td data-label="Source run">
                      <Link to={`runs/${encodeURIComponent(r.run_id)}`}>{r.run_id}</Link>
                      {r.unavailable && (
                        <small className="row-meta">
                          Observation unavailable; retained identity grants no authority.
                        </small>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <Empty title="No execution bindings in this page">This does not prove that no agents are installed.</Empty>
        )}
        <Pagination
          label="Agent observation pagination"
          offset={runs.offset}
          count={runs.runs.length}
          more={runs.has_more}
          onPage={onPage}
        />
      </Panel>
      <Panel title="Installed-agent readiness">
        <div className="product-body">
          <p>
            The read API does not expose the installed registry or its readiness. Inspect the operator configuration
            with <span className="mono">zenchron-engineering autonomy agents --text</span>.
          </p>
          <p className="muted">
            Execution trust grants no acceptance authority. Plan roles and bound profiles are inspected in their plan
            workspace.
          </p>
          <Link to="objectives">Inspect plan assignments</Link>
        </div>
      </Panel>
    </>
  );
}
