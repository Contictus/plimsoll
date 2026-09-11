"use client";

import { Amount } from "../components/Amount";
import { FreshnessBanner } from "../components/Freshness";
import { useEndpoint } from "../lib/session";
import { useLive } from "../components/Live";

type Alert = {
  id: string;
  rule_name: string;
  kind: "fired" | "resolved" | "unavailable";
  metric: string;
  scope_kind: string;
  scope_name: string;
  value: string;
  fired_at: string;
  delivered_at: string | null;
  delivery_error: string;
};

type Rule = {
  id: string;
  name: string;
  metric: string;
  scope_kind: string;
  scope_name: string;
  comparator: string;
  trigger: string;
  clear: string;
  cooldown_seconds: number;
  enabled: boolean;
  firing: boolean;
};

/**
 * Alerts, and the rules behind them.
 *
 * `unavailable` is rendered as its own kind rather than folded into "nothing happened": a rule
 * watching a number that stopped being computed is silent for a reason the user has to be able
 * to tell from safety (L11).
 *
 * The delivery column is here because the alert row exists whether or not the message left the
 * building. An account whose channel expired can see exactly what it was not told.
 */
export default function AlertsPage() {
  const alerts = useEndpoint<{ alerts: Alert[] }>("/alerts");
  const rules = useEndpoint<{ rules: Rule[] }>("/alert-rules");
  useLive("alerts", alerts.reload);

  if (alerts.unauthorized || rules.unauthorized) {
    return (
      <>
        <h1>Alerts</h1>
        <p>
          Your session has ended. <a href="/login">Sign in</a>.
        </p>
      </>
    );
  }

  return (
    <>
      <h1>Alerts</h1>
      <FreshnessBanner
        freshness={{ status: "ok", reasons: [] }}
        asOf="the moment this page was read"
      />

      <h2>Recent</h2>
      {alerts.data === null ? (
        <p className="empty">Reading…</p>
      ) : alerts.data.alerts.length === 0 ? (
        <p className="empty">Nothing has fired.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>When</th>
              <th>Rule</th>
              <th>What</th>
              <th className="num">Value</th>
              <th>Delivery</th>
            </tr>
          </thead>
          <tbody>
            {alerts.data.alerts.map((a) => (
              <tr key={a.id}>
                <td>{a.fired_at}</td>
                <td>{a.rule_name}</td>
                <td>
                  {a.kind === "unavailable"
                    ? `${a.metric} could not be computed`
                    : `${a.scope_name || a.scope_kind} ${a.metric} ${a.kind}`}
                </td>
                <td className="num">
                  <Amount value={a.value} />
                </td>
                <td className={a.delivery_error ? "unknown" : undefined}>
                  {a.delivered_at ? "sent" : a.delivery_error || "not sent"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h2>Rules</h2>
      {rules.data === null ? (
        <p className="empty">Reading…</p>
      ) : rules.data.rules.length === 0 ? (
        <p className="empty">No rules yet.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Metric</th>
              <th>Scope</th>
              <th className="num">Fires</th>
              <th className="num">Clears</th>
              <th>State</th>
            </tr>
          </thead>
          <tbody>
            {rules.data.rules.map((r) => (
              <tr key={r.id}>
                <td>{r.name}</td>
                <td>{r.metric}</td>
                <td>{r.scope_name || r.scope_kind}</td>
                <td className="num">
                  {r.comparator} <Amount value={r.trigger} />
                </td>
                <td className="num">
                  <Amount value={r.clear} />
                </td>
                <td className={r.firing ? "negative" : undefined}>
                  {!r.enabled ? "off" : r.firing ? "firing" : "quiet"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
