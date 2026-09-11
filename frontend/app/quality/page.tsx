"use client";

import type { Freshness } from "../lib/api";
import { Amount } from "../components/Amount";
import { FreshnessBanner } from "../components/Freshness";
import { useEndpoint } from "../lib/session";
import { useLive } from "../components/Live";

type Finding = {
  id: string;
  integration_id: string;
  kind: string;
  subject: string;
  severity: "info" | "warn" | "error";
  detail: string;
  delta: string;
  opened_at: string;
  last_seen_at: string;
  closed_at: string | null;
  occurrences: number;
};

type Register = {
  open: Finding[];
  as_of: string;
  freshness: Freshness;
};

/**
 * The data-quality register: everything we know to be wrong, or cannot account for.
 *
 * This page is the product's claim made checkable. Every other screen says what the numbers
 * are; this one says why you should believe them, and it is the only screen that is more
 * useful the worse the news is.
 *
 * A closed finding is deliberately absent unless asked for. The front page is the present
 * tense: a resolved problem listed beside a live one teaches the reader that neither matters.
 */
export default function QualityPage() {
  const register = useEndpoint<Register>("/data-quality");
  useLive("portfolio", register.reload);

  if (register.unauthorized) {
    return (
      <>
        <h1>Data quality</h1>
        <p>
          Your session has ended. <a href="/login">Sign in</a>.
        </p>
      </>
    );
  }

  if (register.data === null) {
    return (
      <>
        <h1>Data quality</h1>
        <p className="empty">Reading…</p>
      </>
    );
  }

  const findings = register.data.open;

  return (
    <>
      <h1>Data quality</h1>
      <FreshnessBanner
        freshness={register.data.freshness}
        asOf={register.data.as_of}
      />

      {findings.length === 0 ? (
        <p className="empty">
          Nothing outstanding. Every balance we hold agrees with the venue, within the
          tolerance for each asset.
        </p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>What</th>
              <th>Subject</th>
              <th className="num">Delta</th>
              <th>Since</th>
              <th className="num">Seen</th>
              <th>Detail</th>
            </tr>
          </thead>
          <tbody>
            {findings.map((f) => (
              <tr key={f.id}>
                <td className={f.severity === "error" ? "negative" : undefined}>
                  {label(f.kind)}
                </td>
                <td>{f.subject || "—"}</td>
                {/* Amount renders an absent delta as an em dash, never as zero: a problem we
                    could not measure and one measured at zero are opposite claims. */}
                <td className="num">
                  <Amount value={f.delta} />
                </td>
                <td>{f.opened_at}</td>
                <td className="num">{f.occurrences}</td>
                <td>{f.detail}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h2>What these mean</h2>
      <dl>
        <dt>missing event</dt>
        <dd>
          A difference nothing in the ledger explains. This is the one that means we cannot
          account for the number, and the one a resync is the answer to.
        </dd>
        <dt>unsupported</dt>
        <dd>
          A difference a record we deliberately do not ingest accounts for — a fee in an asset
          the registry does not cover, most often. Known, and not a defect.
        </dd>
        <dt>duplicate</dt>
        <dd>
          One venue identity reached the ledger through two connections. This is a defect in
          our own ingest and the only class here that is our fault.
        </dd>
        <dt>rounding</dt>
        <dd>
          A difference smaller than one step of the asset&rsquo;s own precision. A
          representation difference, not a missing fact.
        </dd>
        <dt>snapshot failed</dt>
        <dd>
          We could not ask the venue. Listed rather than ignored, because &ldquo;we could not
          check&rdquo; is not the same claim as &ldquo;we checked and it was fine&rdquo;.
        </dd>
      </dl>
    </>
  );
}

/** label turns a wire code into words, without inventing a meaning the backend did not send. */
function label(kind: string): string {
  return kind.replaceAll("_", " ");
}
