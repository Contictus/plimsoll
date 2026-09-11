"use client";

import { use } from "react";
import type { Envelope } from "../../lib/api";
import { Amount } from "../../components/Amount";
import { Page } from "../../components/Page";

type Event = {
  seq: number;
  venue_event_id: string;
  event_type: string;
  side: string;
  quantity: string;
  price: string;
  fee: string;
  fee_asset: string;
  event_time: string;
};

type Lineage = Envelope & {
  position: { id: string; symbol: string; quantity: string; avg_entry_price: string };
  events: Event[];
};

/**
 * One position, opened down to the events that produced it.
 *
 * This page is the product's claim made literal: the number at the top is not asserted, it is
 * derived from the rows underneath it, and every row names the venue event it came from.
 */
export default function PositionPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = use(params);
  return (
    <Page<Lineage> title="Position" path={`/positions/${id}/lineage`} topic="positions">
      {(data) => (
        <>
          <div className="cards">
            <div className="card">
              <div className="label">Symbol</div>
              <div className="value">{data.position.symbol}</div>
            </div>
            <div className="card">
              <div className="label">Quantity</div>
              <div className="value">
                <Amount value={data.position.quantity} places={8} />
              </div>
            </div>
            <div className="card">
              <div className="label">Average entry</div>
              <div className="value">
                <Amount value={data.position.avg_entry_price} />
              </div>
            </div>
          </div>

          <h2>The events behind it</h2>
          <table>
            <thead>
              <tr>
                <th>When</th>
                <th>Type</th>
                <th>Side</th>
                <th className="num">Quantity</th>
                <th className="num">Price</th>
                <th className="num">Fee</th>
                <th>Venue id</th>
              </tr>
            </thead>
            <tbody>
              {data.events.map((e) => (
                <tr key={e.venue_event_id}>
                  <td>{e.event_time}</td>
                  <td>{e.event_type}</td>
                  <td>{e.side || "—"}</td>
                  <td className="num">
                    <Amount value={e.quantity} places={8} />
                  </td>
                  <td className="num">
                    <Amount value={e.price} />
                  </td>
                  <td className="num">
                    <Amount value={e.fee} places={8} /> {e.fee_asset}
                  </td>
                  <td>
                    <code>{e.venue_event_id}</code>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </Page>
  );
}
