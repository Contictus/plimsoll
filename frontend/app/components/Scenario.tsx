"use client";

import { useState } from "react";
import { post, Refused, Unauthorized } from "../lib/api";
import { Amount } from "./Amount";

type Outcome = {
  equity: string;
  unrealized_pnl: string;
  margin_balance: string;
  maintenance_margin: string;
  buffer: string;
  liquidated: boolean;
};

type Projection = {
  base: Outcome;
  shocked: Outcome;
  shocks: Record<string, string>;
  unavailable: string[];
};

/**
 * The scenario form: what happens to this account if one asset moves.
 *
 * The asset is typed in rather than picked from a list on purpose. A shock names its asset and
 * the unshocked hold still (K56), so the field is the question: nothing here invents a
 * correlation, and a user who wants two assets shocked says so twice.
 *
 * The move is a string all the way to the wire. It is a number that decides a liquidation, and
 * this dashboard parses none of those (K52, L1).
 */
export function Scenario() {
  const [asset, setAsset] = useState("BTC");
  const [move, setMove] = useState("-0.2");
  const [result, setResult] = useState<Projection | null>(null);
  const [refused, setRefused] = useState<string>("");
  const [pending, setPending] = useState(false);

  async function project() {
    setPending(true);
    setRefused("");
    try {
      setResult(
        await post<Projection>("/risk/scenario", {
          shocks: { [asset.trim().toUpperCase()]: move.trim() },
        }),
      );
    } catch (error) {
      setResult(null);
      if (error instanceof Unauthorized) {
        window.location.href = "/login";
        return;
      }
      setRefused(
        error instanceof Refused ? error.message : "the projection could not be read",
      );
    } finally {
      setPending(false);
    }
  }

  return (
    <section>
      <h2>Scenario</h2>
      <p className="empty">
        Move one asset and see where this account lands. Everything you do not name holds
        still — nothing here guesses how two assets move together.
      </p>

      <p>
        <label>
          Asset{" "}
          <input
            value={asset}
            onChange={(e) => setAsset(e.target.value)}
            aria-label="asset"
          />
        </label>{" "}
        <label>
          Move{" "}
          <input
            value={move}
            onChange={(e) => setMove(e.target.value)}
            aria-label="move"
          />
        </label>{" "}
        <button onClick={project} disabled={pending}>
          {pending ? "Projecting…" : "Project"}
        </button>
      </p>
      <p className="empty">
        A move is a fraction: <code>-0.2</code> is minus twenty percent.
      </p>

      {refused ? <p className="negative">{refused}</p> : null}

      {result === null ? null : (
        <>
          <table>
            <thead>
              <tr>
                <th></th>
                <th className="num">Equity</th>
                <th className="num">Margin balance</th>
                <th className="num">Maintenance</th>
                <th className="num">Buffer</th>
              </tr>
            </thead>
            <tbody>
              <Row label="Now" outcome={result.base} />
              <Row label="After" outcome={result.shocked} />
            </tbody>
          </table>

          {result.shocked.liquidated ? (
            <p className="negative">
              This scenario takes the account past its maintenance requirement.
            </p>
          ) : null}

          {result.unavailable.length > 0 ? (
            <p className="unknown">
              Not accounted for: {result.unavailable.join(", ")}. A missing bracket table
              leaves the buffer unknown rather than larger — it would otherwise report this
              account as safer than it is.
            </p>
          ) : null}
        </>
      )}
    </section>
  );
}

function Row({ label, outcome }: { label: string; outcome: Outcome }) {
  return (
    <tr>
      <td>{label}</td>
      <td className="num">
        <Amount value={outcome.equity} />
      </td>
      <td className="num">
        <Amount value={outcome.margin_balance} />
      </td>
      {/* Amount renders an absent value as an em dash. An unknown requirement and a zero one
          are opposite claims, and a buffer shown as 0 reads as "right on the line". */}
      <td className="num">
        <Amount value={outcome.maintenance_margin} />
      </td>
      <td className={outcome.liquidated ? "num negative" : "num"}>
        <Amount value={outcome.buffer} />
      </td>
    </tr>
  );
}
