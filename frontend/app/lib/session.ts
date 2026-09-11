"use client";

import { useCallback, useEffect, useState } from "react";
import { Unauthorized, get } from "./api";

/**
 * useAccount is every page's session check, and the whole of the client's auth code.
 *
 * There is no token here to store or refresh: the cookie is HttpOnly and same-origin (K16,
 * K27), so a 401 is the only thing this side can observe about a session, and the answer to
 * one is the login form.
 */
export function useAccount(): { ready: boolean; authenticated: boolean } {
  const [state, setState] = useState({ ready: false, authenticated: false });

  useEffect(() => {
    let live = true;
    get<{ email: string }>("/me")
      .then(() => live && setState({ ready: true, authenticated: true }))
      .catch(() => live && setState({ ready: true, authenticated: false }));
    return () => {
      live = false;
    };
  }, []);

  return state;
}

/**
 * useEndpoint reads one endpoint and re-reads on demand. Errors are kept rather than thrown:
 * a page that renders nothing when a read fails tells the user less than one that says the
 * read failed.
 */
export function useEndpoint<T>(path: string): {
  data: T | null;
  error: string | null;
  unauthorized: boolean;
  reload: () => void;
} {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [unauthorized, setUnauthorized] = useState(false);
  const [tick, setTick] = useState(0);

  useEffect(() => {
    let live = true;
    get<T>(path)
      .then((value) => {
        if (!live) return;
        setData(value);
        setError(null);
      })
      .catch((err: unknown) => {
        if (!live) return;
        if (err instanceof Unauthorized) {
          setUnauthorized(true);
          return;
        }
        setError(err instanceof Error ? err.message : "the read failed");
      });
    return () => {
      live = false;
    };
  }, [path, tick]);

  const reload = useCallback(() => setTick((t) => t + 1), []);
  return { data, error, unauthorized, reload };
}
