"use client";

import type { Freshness, Status } from "../lib/api";

/**
 * The freshness banner, built before any number is rendered.
 *
 * Every page renders it. A dashboard that shows totals without showing what they are missing
 * is exactly the product this repository argues against (L11) -- and building this component
 * last is how it ends up bolted onto two pages out of five.
 *
 * The three statuses are three different visual states, never a shade of the same one: a
 * reader who cannot tell `degraded` from `unreliable` at a glance has been given a decoration
 * rather than a warning.
 */
const label: Record<Status, string> = {
  ok: "Current",
  degraded: "Degraded",
  unreliable: "Unreliable",
};

export function FreshnessBanner({
  freshness,
  asOf,
}: {
  freshness: Freshness;
  asOf: string;
}) {
  const reasons = freshness.reasons ?? [];
  return (
    <section className={`freshness freshness-${freshness.status}`}>
      <header>
        <strong>{label[freshness.status]}</strong>
        <span className="as-of">as of {asOf}</span>
      </header>
      {reasons.length > 0 && (
        <ul>
          {reasons.map((reason, i) => (
            <li key={`${reason.code}-${i}`} className={`severity-${reason.severity}`}>
              <code>{reason.code}</code> {reason.detail}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
