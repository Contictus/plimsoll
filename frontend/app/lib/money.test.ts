import { strict as assert } from "node:assert";
import { test } from "node:test";
import { UNKNOWN, isNegative, money, percent } from "./money.ts";

// L1 AT THE LAST MILE.
//
// The backend goes to considerable trouble to keep these digits exact. If the dashboard parses
// them, all of it is undone in the last thirty pixels: JavaScript's number is a float64, and it
// cannot hold what NUMERIC(38,18) holds.
test("a value larger than float64 can represent survives intact", () => {
  const huge = "12345678901234567890.123456789012345678";
  assert.equal(money(huge, 18), "12,345,678,901,234,567,890.123456789012345678");
});

test("the classic float failure never happens, because nothing is added", () => {
  assert.equal(money("0.30000000000000004", 2), "0.3");
  assert.equal(money("1000000000000000000000", 2), "1,000,000,000,000,000,000,000");
});

// L11 and K50 at the last mile: absent and zero must not render the same.
test("an absent number is not a zero", () => {
  assert.equal(money(""), UNKNOWN);
  assert.equal(money(null), UNKNOWN);
  assert.equal(money(undefined), UNKNOWN);
  assert.equal(money("0"), "0");
  assert.notEqual(money(""), money("0"));
});

test("a negative renders with its sign, and a negative zero does not", () => {
  assert.equal(money("-1234.5"), "-1,234.5");
  assert.equal(money("-0.000"), "0");
  assert.equal(isNegative("-0.000"), false);
  assert.equal(isNegative("-0.001"), true);
});

test("trailing zeros are trimmed but digits are never rounded up", () => {
  assert.equal(money("1.500"), "1.5");
  // Truncated, not rounded: rounding at the display layer would show a number the API did not
  // send, and the difference would be invisible.
  assert.equal(money("1.999", 2), "1.99");
});

test("a ratio becomes a percentage without leaving the string domain", () => {
  assert.equal(percent("0.2"), "20%");
  assert.equal(percent("0.0125", 2), "1.25%");
  assert.equal(percent("1"), "100%");
  // An unknown ratio renders as the unknown mark alone: "—%" reads as a percentage that
  // happens to be missing its digits, and this is not a percentage at all.
  assert.equal(percent(""), UNKNOWN);
});

test("nonsense is unknown rather than a number", () => {
  for (const bad of ["abc", "1.2.3", "--1", "1e21"]) {
    assert.equal(money(bad), UNKNOWN, `${bad} was rendered as a number`);
  }
});
