"use client";

import type { Envelope } from "../lib/api";
import { Amount } from "../components/Amount";
import { Page } from "../components/Page";
import { UNKNOWN } from "../lib/money";

type RiskPosition = {
  instrument_id: number;
  symbol: string;
  quantity: string;
  entry_price: string;
  mark_price: string;
  liquidation_price: string;
  notional: string;
  leverage: string;
  maint_margin: string;
  liquidation_distance: string;
};

type IntegrationRisk = {
  integration_id: string;
  exchange: string;
  label: string;
  as_of: string;
  margin_balance: string;
  maintenance_margin: string;
  available_balance: string;
  unrealized_pnl: string;
  margin_buffer: string;
  positions: RiskPosition[];
};

type Risk = Envelope & { integrations: IntegrationRisk[] };

/**
 * The page this user opens.
 *
 * Two states must never render the same: a margin buffer that IS zero, and one nobody could
 * compute. The first says liquidation is imminent; the second says nothing at all (K50). The
 * dash below is the second, and an integration with no capture does not appear here at all --
 * it appears in the freshness banner as collateral_unavailable, which is where an absence
 * belongs.
 */
export default function RiskPage() {
  return (
    <Page<Risk> title="Risk" path="/risk" topic="risk">
      {(data) =>
        data.integrations.length === 0 ? (
          <p className="empty">
            No margin picture has been captured. The banner above says why — an empty screen
            here is not an account with no risk.
          </p>
        ) : (
          data.integrations.map((one) => (
            <section key={one.integration_id}>
              <h2>
                {one.exchange} {one.label}
              </h2>
              <div className="cards">
                <div className="card">
                  <div className="label">Margin buffer</div>
                  <div className="value">
                    <Amount value={one.margin_buffer} />
                  </div>
                </div>
                <div className="card">
                  <div className="label">Margin balance</div>
                  <div className="value">
                    <Amount value={one.margin_balance} />
                  </div>
                </div>
                <div className="card">
                  <div className="label">Maintenance margin</div>
                  <div className="value">
                    <Amount value={one.maintenance_margin} />
                  </div>
                </div>
                <div className="card">
                  <div className="label">Unrealized PnL</div>
                  <div className="value">
                    <Amount value={one.unrealized_pnl} />
                  </div>
                </div>
              </div>

              {one.positions.length === 0 ? (
                <p className="empty">No open positions.</p>
              ) : (
                <table>
                  <thead>
                    <tr>
                      <th>Symbol</th>
                      <th className="num">Quantity</th>
                      <th className="num">Mark</th>
                      <th className="num">Liquidation</th>
                      <th className="num">Distance</th>
                      <th className="num">Notional</th>
                      <th className="num">Leverage</th>
                    </tr>
                  </thead>
                  <tbody>
                    {one.positions.map((p) => (
                      <tr key={`${one.integration_id}-${p.instrument_id}`}>
                        <td>{p.symbol}</td>
                        <td className="num">
                          <Amount value={p.quantity} places={8} />
                        </td>
                        <td className="num">
                          <Amount value={p.mark_price} />
                        </td>
                        <td className="num">
                          {p.liquidation_price === "0" ? (
                            <span className="unknown" title="the venue reports none">
                              {UNKNOWN}
                            </span>
                          ) : (
                            <Amount value={p.liquidation_price} />
                          )}
                        </td>
                        <td className="num">
                          <Amount value={p.liquidation_distance} ratio />
                        </td>
                        <td className="num">
                          <Amount value={p.notional} />
                        </td>
                        <td className="num">
                          <Amount value={p.leverage} />×
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </section>
          ))
        )
      }
    </Page>
  );
}
