"use client";

import type { Envelope } from "./lib/api";
import { Amount } from "./components/Amount";
import { Page } from "./components/Page";

type Position = {
  id: string;
  symbol: string;
  kind: string;
  quantity: string;
  avg_entry_price: string;
  cost_basis: string;
  realized_pnl: string;
  market_value: string;
  unrealized_pnl: string;
  strategy: string;
  flat: boolean;
};

type Balance = {
  asset: string;
  quantity: string;
  value_usd: string;
  negative: boolean;
};

type Portfolio = Envelope & {
  total_value_usd: string;
  unpriced_assets: string[];
  positions: Position[];
  balances: Balance[];
};

export default function PortfolioPage() {
  return (
    <Page<Portfolio> title="Portfolio" path="/portfolio" topic="portfolio">
      {(data) => (
        <>
          <div className="cards">
            <div className="card">
              <div className="label">Total value (USD)</div>
              <div className="value">
                <Amount value={data.total_value_usd} />
              </div>
            </div>
            <div className="card">
              <div className="label">Positions</div>
              <div className="value">{data.positions.filter((p) => !p.flat).length}</div>
            </div>
            <div className="card">
              <div className="label">Unpriced assets</div>
              <div className="value">{data.unpriced_assets.length}</div>
            </div>
          </div>

          <h2>Positions</h2>
          {data.positions.length === 0 ? (
            <p className="empty">No positions yet.</p>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Symbol</th>
                  <th>Strategy</th>
                  <th className="num">Quantity</th>
                  <th className="num">Avg entry</th>
                  <th className="num">Market value</th>
                  <th className="num">Unrealized</th>
                  <th className="num">Realized</th>
                </tr>
              </thead>
              <tbody>
                {data.positions.map((p) => (
                  <tr key={p.id}>
                    <td>
                      <a href={`/positions/${p.id}`}>{p.symbol}</a>
                    </td>
                    <td>{p.strategy || "—"}</td>
                    <td className="num">
                      <Amount value={p.quantity} places={8} />
                    </td>
                    <td className="num">
                      <Amount value={p.avg_entry_price} />
                    </td>
                    <td className="num">
                      <Amount value={p.market_value} />
                    </td>
                    <td className="num">
                      <Amount value={p.unrealized_pnl} />
                    </td>
                    <td className="num">
                      <Amount value={p.realized_pnl} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}

          <h2>Balances</h2>
          {data.balances.length === 0 ? (
            <p className="empty">No balances yet.</p>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Asset</th>
                  <th className="num">Quantity</th>
                  <th className="num">Value (USD)</th>
                </tr>
              </thead>
              <tbody>
                {data.balances.map((b) => (
                  <tr key={`${b.asset}-${b.quantity}`}>
                    <td>{b.asset}</td>
                    <td className={b.negative ? "num negative" : "num"}>
                      <Amount value={b.quantity} places={8} />
                    </td>
                    <td className="num">
                      <Amount value={b.value_usd} />
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
