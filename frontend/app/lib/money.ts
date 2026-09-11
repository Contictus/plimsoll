/**
 * Money formatting, done on strings.
 *
 * Nothing here parses. The backend sends "12345678901234567890.123456789012345678" and this
 * file groups its digits and trims its zeros without ever handing it to JavaScript's number
 * type -- which is a float64, cannot hold that value, and would print it as 1.2345678901234568e+19.
 *
 * A dashboard that rounds the numbers a ledger went to this much trouble to keep exact is a
 * dashboard that undoes the product's only claim (L1).
 */

/** A number the API could not compute at all, rendered so it cannot be mistaken for zero. */
export const UNKNOWN = "—";

type Parts = { negative: boolean; whole: string; fraction: string };

function split(value: string): Parts | null {
  const trimmed = value.trim();
  if (trimmed === "") return null;
  const negative = trimmed.startsWith("-");
  const unsigned = negative ? trimmed.slice(1) : trimmed;
  if (!/^\d+(\.\d+)?$/.test(unsigned)) return null;
  const [whole, fraction = ""] = unsigned.split(".");
  return { negative, whole, fraction };
}

/** group inserts thin separators every three digits, left of the point only. */
function group(whole: string): string {
  return whole.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

/**
 * money renders an amount with at most `places` decimals, trimming trailing zeros. An empty
 * or unparsable input renders as UNKNOWN rather than as 0: the API sends "" for a number it
 * could not compute, and zero and unknown are opposite claims (L11, K50).
 */
export function money(value: string | null | undefined, places = 2): string {
  if (value === null || value === undefined) return UNKNOWN;
  const parts = split(value);
  if (parts === null) return UNKNOWN;

  let fraction = parts.fraction.slice(0, places);
  while (fraction.endsWith("0")) fraction = fraction.slice(0, -1);

  const body = fraction === "" ? group(parts.whole) : `${group(parts.whole)}.${fraction}`;
  return parts.negative && /[1-9]/.test(parts.whole + parts.fraction) ? `-${body}` : body;
}

/** percent renders a ratio as a percentage, again without leaving the string domain. */
export function percent(value: string | null | undefined, places = 2): string {
  const parts = value === null || value === undefined ? null : split(value);
  if (parts === null) return UNKNOWN;

  // Multiplying by 100 is a shift of the decimal point, which is an edit to a string. The
  // point moves two places right; the digits themselves are never touched, so a ratio with
  // eighteen decimals loses nothing on its way to a percentage.
  const digits = parts.whole + parts.fraction;
  const point = parts.whole.length + 2;
  const padded = digits.padEnd(point, "0");
  const whole = padded.slice(0, point).replace(/^0+(?=\d)/, "");
  const fraction = padded.slice(point);
  // Appended only when there is one: "20." is not a number, and money() would read it as
  // nonsense and render the whole thing unknown -- a formatting slip turning into a claim
  // that the value could not be computed.
  const shifted = fraction === "" ? whole : `${whole}.${fraction}`;
  return `${money(parts.negative ? `-${shifted}` : shifted, places)}%`;
}

/** isNegative answers from the sign character, not from a comparison. */
export function isNegative(value: string | null | undefined): boolean {
  if (!value) return false;
  const parts = split(value);
  return parts !== null && parts.negative && /[1-9]/.test(parts.whole + parts.fraction);
}
