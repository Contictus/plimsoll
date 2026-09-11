"use client";

import { UNKNOWN, isNegative, money, percent } from "../lib/money";

/**
 * Amount renders one number, and renders an ABSENT one differently from a zero.
 *
 * That distinction is the whole reason this component exists rather than an inline call. The
 * API sends "" for a number it could not compute -- an unpriced position, a margin buffer with
 * no capture behind it -- and a screen that showed "0" there would be making the opposite
 * claim to the one the response is making (L11, K50).
 */
export function Amount({
  value,
  places = 2,
  ratio = false,
}: {
  value: string | null | undefined;
  places?: number;
  ratio?: boolean;
}) {
  const text = ratio ? percent(value, places) : money(value, places);
  const unknown = text === UNKNOWN || text === `${UNKNOWN}%`;
  const className = unknown ? "unknown" : isNegative(value) ? "negative" : undefined;
  return (
    <span className={className} title={unknown ? "not computable from this response" : value ?? ""}>
      {unknown ? UNKNOWN : text}
    </span>
  );
}
