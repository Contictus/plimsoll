"use client";

import { useState } from "react";

/**
 * Sign in.
 *
 * There is no token handling here and nothing is stored: the API sets an HttpOnly cookie and
 * the browser sends it back (K16, K27). This form posts and navigates; that is the whole of
 * the client's part in authentication, and it is why there is no way for a script on this page
 * to read a session.
 */
export default function LoginPage() {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    const response = await fetch("/api/auth/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "same-origin",
      body: JSON.stringify({ email, password }),
    });
    setBusy(false);
    if (!response.ok) {
      // One message for every failure, matching the API: a wrong password and an unknown
      // address must not be distinguishable, here or there.
      setError("Those credentials were not accepted.");
      return;
    }
    window.location.href = "/";
  }

  return (
    <>
      <h1>Sign in</h1>
      <form className="login" onSubmit={submit}>
        <input
          type="email"
          placeholder="you@example.com"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          autoComplete="username"
          required
        />
        <input
          type="password"
          placeholder="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          autoComplete="current-password"
          required
        />
        <button type="submit" disabled={busy}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
        {error && <p className="error">{error}</p>}
      </form>
    </>
  );
}
