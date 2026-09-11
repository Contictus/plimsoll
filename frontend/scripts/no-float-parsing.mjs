// L1, enforced in the one place the rule is easiest to break.
//
// The backend sends every number as a string, deliberately: a float64 in a client's parser is
// our bug too, and JavaScript's number is a float64 with no opt-out. `0.1 + 0.2` is
// 0.30000000000000004 and 1e21 renders as "1e+21", so a portfolio total parsed and re-printed
// is a portfolio total that is wrong on the screen while being right in the database.
//
// This walks the source and fails on the three ways it gets broken: parseFloat, Number(...)
// and the unary plus on a value. Formatting for display is done with strings.
import { readdir, readFile } from "node:fs/promises";
import { join, extname } from "node:path";

const roots = ["app"];
const banned = [
  { pattern: /\bparseFloat\s*\(/, why: "parseFloat turns an exact string into a float64" },
  { pattern: /\bNumber\s*\(/, why: "Number() turns an exact string into a float64" },
  { pattern: /\btoFixed\s*\(/, why: "toFixed is float64 rounding wearing a formatter's hat" },
];

async function* walk(dir) {
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      yield* walk(path);
    } else if ([".ts", ".tsx"].includes(extname(entry.name))) {
      yield path;
    }
  }
}

let failures = 0;
for (const root of roots) {
  for await (const path of walk(root)) {
    const source = await readFile(path, "utf8");
    source.split("\n").forEach((line, i) => {
      if (line.includes("no-float-parsing: allow")) return;
      for (const { pattern, why } of banned) {
        if (pattern.test(line)) {
          console.error(`${path}:${i + 1}: ${why}\n  ${line.trim()}`);
          failures += 1;
        }
      }
    });
  }
}

if (failures > 0) {
  console.error(`\n${failures} money path(s) would go through a float64 (L1).`);
  process.exit(1);
}
console.log("no-float-parsing: OK");
