"use client";

import type { Freshness } from "../lib/api";
import { Amount } from "../components/Amount";
import { FreshnessBanner } from "../components/Freshness";
import { useEndpoint } from "../lib/session";

type Transfer = {
  id: string;
  asset: string;
  method: "txid" | "heuristic" | "manual";
  out_exchange: string;
  out_quantity: string;
  out_event_time: string;
  in_exchange: string;
  in_quantity: string;
  in_event_time: string;
  linked_at: string;
};

type Transfers = {
  transfers: Transfer[];
  as_of: string;
  freshness: Freshness;
};

const method: Record<Transfer["method"], string> = {
  txid: "chain transaction",
  heuristic: "amount and time",
  manual: "you said so",
};

/**
 * Movements between venues, joined.
 *
 * A link changes no number — the deposit already added and the withdrawal already subtracted,
 * on two different integrations, and both were right. What it changes is the reading: a
 * withdrawal taken for a disposal invents a realized loss you never took. So this page shows
 * the two halves side by side and how they were joined, because "how" is the part a user has
 * to be able to disagree with.
 *
 * The fee is not a column. It is the difference between the two quantities, shown rather than
 * asserted: a single figure would be our arithmetic presented as the venue's fact.
 *
 * Legs with no other half are not here. They are in the data-quality register, because an
 * unmatched leg is a problem with the numbers rather than a transfer (K57).
 */
export default function TransfersPage() {
  const data = useEndpoint<Transfers>("/transfers");

  if (data.unauthorized) {
    return (
      <>
        <h1>Transfers</h1>
        <p>
          Your session has ended. <a href="/login">Sign in</a>.
        </p>
      </>
    );
  }

  if (data.data === null) {
    return (
      <>
        <h1>Transfers</h1>
        <p className="empty">Reading…</p>
      </>
    );
  }

  return (
    <>
      <h1>Transfers</h1>
      <FreshnessBanner freshness={data.data.freshness} asOf={data.data.as_of} />

      {data.data.transfers.length === 0 ? (
        <p className="empty">
          Nothing joined yet. A withdrawal from one venue and the deposit it becomes on another
          are matched automatically; anything the matcher will not guess at is listed in{" "}
          <a href="/quality">data quality</a> for you to settle.
        </p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Asset</th>
              <th>Left</th>
              <th className="num">Sent</th>
              <th>Arrived</th>
              <th className="num">Received</th>
              <th>Joined by</th>
            </tr>
          </thead>
          <tbody>
            {data.data.transfers.map((one) => (
              <tr key={one.id}>
                <td>{one.asset}</td>
                <td>
                  {one.out_exchange}
                  <br />
                  <span className="empty">{one.out_event_time}</span>
                </td>
                <td className="num">
                  <Amount value={one.out_quantity} />
                </td>
                <td>
                  {one.in_exchange}
                  <br />
                  <span className="empty">{one.in_event_time}</span>
                </td>
                <td className="num">
                  <Amount value={one.in_quantity} />
                </td>
                <td className={one.method === "heuristic" ? "unknown" : undefined}>
                  {method[one.method]}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h2>Why this page exists</h2>
      <p className="empty">
        Unmatched, a withdrawal reads as a sale and the deposit that follows it reads as a
        purchase at no cost. Your realized profit collapses and your cost basis is invented.
        Joining the two halves is what stops that — it is the industry&rsquo;s most common
        support question, and the numbers here are the answer to it.
      </p>
      <p className="empty">
        A join made from a chain transaction is proof. One made from the amount and the time is
        a strong guess, marked as such — if it is wrong, unjoin it.
      </p>
    </>
  );
}
