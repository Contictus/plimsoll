"use client";

import type { ReactNode } from "react";
import type { Envelope } from "../lib/api";
import { FreshnessBanner } from "./Freshness";
import { useEndpoint } from "../lib/session";
import { useLive } from "./Live";

/**
 * Page is the shape every screen here has: read one endpoint, render the freshness of what
 * came back, then render the numbers -- in that order, always.
 *
 * The order is the point. A component that renders numbers first and the banner "somewhere
 * above" is one refactor away from a screen with totals and no qualification, which is the
 * failure L11 exists to prevent.
 */
export function Page<T extends Envelope>({
  title,
  path,
  topic,
  children,
}: {
  title: string;
  path: string;
  topic: string;
  children: (data: T) => ReactNode;
}) {
  const { data, error, unauthorized, reload } = useEndpoint<T>(path);
  useLive(topic, reload);

  if (unauthorized) {
    return (
      <>
        <h1>{title}</h1>
        <p>
          Your session has ended. <a href="/login">Sign in</a>.
        </p>
      </>
    );
  }
  if (error) {
    return (
      <>
        <h1>{title}</h1>
        <p className="error">{error}</p>
      </>
    );
  }
  if (data === null) {
    return (
      <>
        <h1>{title}</h1>
        <p className="empty">Reading…</p>
      </>
    );
  }

  return (
    <>
      <h1>{title}</h1>
      <FreshnessBanner freshness={data.freshness} asOf={data.as_of} />
      {children(data)}
    </>
  );
}
