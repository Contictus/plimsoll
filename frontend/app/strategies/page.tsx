"use client";

import type { Envelope } from "../lib/api";
import { Amount } from "../components/Amount";
import { Page } from "../components/Page";

type Metrics = {
  gross_exposure: string;
  net_exposure: string;
  leverage: string;
  net_leverage: string;
  net_delta: { asset: string; exposure: string }[];
};

type Exposure = Envelope & {
  equity: string;
  portfolio: Metrics;
  by_strategy: { strategy: string; metrics: Metrics }[];
  unpriced_assets: string[];
};

/**
 * Exposure, per strategy.
 *
 * Gross and net leverage are shown side by side because their difference IS the hedge: a
 * delta-neutral basis trade reads 2x gross and 0 net, and a screen that showed only the first
 * would tell this user they are twice as exposed as they are -- every day, until they stopped
 * reading it (K13).
 */
export default function StrategiesPage() {
  return (
    <Page<Exposure> title="Exposure" path="/exposure" topic="portfolio">
      {(data) => (
        <>
          <div className="cards">
            <div className="card">
              <div className="label">Equity</div>
              <div className="value">
                <Amount value={data.equity} />
              </div>
            </div>
            <div className="card">
              <div className="label">Gross exposure</div>
              <div className="value">
                <Amount value={data.portfolio.gross_exposure} />
              </div>
            </div>
            <div className="card">
              <div className="label">Gross leverage</div>
              <div className="value">
                <Amount value={data.portfolio.leverage} />×
              </div>
            </div>
            <div className="card">
              <div className="label">Directional leverage</div>
              <div className="value">
                <Amount value={data.portfolio.net_leverage} />×
              </div>
            </div>
          </div>

          <h2>By strategy</h2>
          {data.by_strategy.length === 0 ? (
            <p className="empty">Nothing grouped yet.</p>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Strategy</th>
                  <th className="num">Gross</th>
                  <th className="num">Net</th>
                  <th className="num">Gross leverage</th>
                  <th className="num">Directional</th>
                  <th>Net delta</th>
                </tr>
              </thead>
              <tbody>
                {data.by_strategy.map((s) => (
                  <tr key={s.strategy || "ungrouped"}>
                    <td>{s.strategy || "ungrouped"}</td>
                    <td className="num">
                      <Amount value={s.metrics.gross_exposure} />
                    </td>
                    <td className="num">
                      <Amount value={s.metrics.net_exposure} />
                    </td>
                    <td className="num">
                      <Amount value={s.metrics.leverage} />×
                    </td>
                    <td className="num">
                      <Amount value={s.metrics.net_leverage} />×
                    </td>
                    <td>
                      {s.metrics.net_delta
                        .map((d) => `${d.asset} ${d.exposure}`)
                        .join(", ") || "—"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </>
      )}
    </Page>
  );
}
