import { useEffect, useState } from "react";
import * as workspace from "./workspace-api";
import { Empty, Panel } from "./components";
export function InstalledAgents() {
  const [options, setOptions] = useState<workspace.Options | null>(null),
    [error, setError] = useState(""),
    [reload, setReload] = useState(0);
  useEffect(() => {
    const owner = new AbortController();
    setOptions(null);
    setError("");
    workspace
      .options(owner.signal)
      .then(setOptions)
      .catch((e) => {
        if (!owner.signal.aborted) setError(e instanceof Error ? e.message : "Options unavailable");
      });
    return () => owner.abort();
  }, [reload]);
  return (
    <>
      <Panel
        title="Installed execution agents"
        extra={
          <button className="button" onClick={() => setReload((v) => v + 1)}>
            Refresh installed configuration
          </button>
        }
      >
        <div className="product-body">
          <p>Controller-owned configuration. Eligibility grants no acceptance or publication authority.</p>
          {error ? (
            <p role="alert">Installed inventory unavailable · {error}</p>
          ) : !options ? (
            <p role="status">Reading installed agents…</p>
          ) : options.agents.length ? (
            <div className="table-scroll" tabIndex={0} role="region" aria-label="Installed agent inventory">
              <table className="data-table">
                <thead>
                  <tr>
                    {["Agent identity", "Provider / model", "Execution trust", "Eligibility"].map((h) => (
                      <th key={h} scope="col">
                        {h}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {options.agents.map((a) => (
                    <tr key={a.id}>
                      <td data-label="Agent identity">
                        {a.id}
                        {a.default ? " · default" : ""}
                      </td>
                      <td data-label="Provider / model">
                        {a.kind} / {a.model || "Default not reported"}
                      </td>
                      <td data-label="Execution trust">{a.trust_mode}</td>
                      <td data-label="Eligibility">{a.eligible ? "Eligible by configuration" : "Not eligible"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <Empty title="No installed agents reported">This is the current configuration response.</Empty>
          )}
        </div>
      </Panel>
      <Panel title="Agent profiles & plan templates">
        <div className="product-body">
          {options ? (
            <>
              <h3>Profiles</h3>
              <ul>
                {options.profiles.map((p) => (
                  <li className="mono" key={p}>
                    {p}
                  </li>
                ))}
              </ul>
              <h3>Templates</h3>
              <ul>
                {options.templates.map((t) => (
                  <li className="mono" key={t}>
                    {t}
                  </li>
                ))}
              </ul>
              <p>These specialize existing workers and planning inputs; they grant no authority.</p>
            </>
          ) : (
            <p>Configuration has not been observed.</p>
          )}
        </div>
      </Panel>
    </>
  );
}
