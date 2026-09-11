/**
 * The one place this dashboard talks to the API.
 *
 * Every request is same-origin and relative (K27): the session cookie is HttpOnly, the
 * browser attaches it, and there is no token in JavaScript to store, refresh or leak. That is
 * the whole authentication story on this side, which is why there is no auth code here.
 */

export type Severity = "info" | "warn" | "error";
export type Status = "ok" | "degraded" | "unreliable";

export type Reason = {
  code: string;
  severity: Severity;
  detail: string;
  since: string;
};

export type Freshness = {
  status: Status;
  reasons: Reason[];
};

export type Envelope = {
  as_of: string;
  freshness: Freshness;
};

/** Unauthorized is what every page turns into a redirect to the login form. */
export class Unauthorized extends Error {
  constructor() {
    super("unauthorized");
  }
}

/**
 * get reads one endpoint. It never caches: a risk page served from a cache is a margin buffer
 * from a moment that has passed, and the freshness envelope it carries would be describing
 * that moment rather than this one (L11).
 */
export async function get<T>(path: string): Promise<T> {
  const response = await fetch(`/api${path}`, {
    cache: "no-store",
    credentials: "same-origin",
  });
  if (response.status === 401) {
    throw new Unauthorized();
  }
  if (!response.ok) {
    throw new Error(`${path} answered ${response.status}`);
  }
  return (await response.json()) as T;
}

/**
 * post sends a body and reads the answer. Only one endpoint needs it: the scenario projection,
 * which carries a body and therefore cannot be a GET (L13 -- it models and writes nothing).
 *
 * A 422 is returned as its own error rather than thrown as a failure, because "that move would
 * take the price below zero" is an answer to the question, not a broken request, and the user
 * has to be able to read it.
 */
export class Refused extends Error {}

export async function post<T>(path: string, body: unknown): Promise<T> {
  const response = await fetch(`/api${path}`, {
    method: "POST",
    cache: "no-store",
    credentials: "same-origin",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  if (response.status === 401) {
    throw new Unauthorized();
  }
  if (response.status === 422) {
    const detail = (await response.json()) as { detail?: string };
    throw new Refused(detail.detail ?? "that scenario cannot be projected");
  }
  if (!response.ok) {
    throw new Error(`${path} answered ${response.status}`);
  }
  return (await response.json()) as T;
}
