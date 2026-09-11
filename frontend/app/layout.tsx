import type { ReactNode } from "react";
import "./globals.css";

export const metadata = {
  title: "Plimsoll",
  description: "The numbers are right, and we can prove it.",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>
        <nav className="top">
          <span className="brand">Plimsoll</span>
          <a href="/">Portfolio</a>
          <a href="/risk">Risk</a>
          <a href="/strategies">Strategies</a>
          <a href="/alerts">Alerts</a>
          <a href="/transfers">Transfers</a>
          <a href="/quality">Data quality</a>
        </nav>
        <main>{children}</main>
      </body>
    </html>
  );
}
