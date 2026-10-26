import { describe, expect, it } from "vitest";

// Q10: a new language is a new dictionary, with no component changes. That
// only holds while no Russian is written outside ru.ts, so any Cyrillic left
// in the code (comments aside) fails here with where it is.
const sources = import.meta.glob<string>(["../**/*.{ts,tsx}", "!./ru.ts", "!../**/*.test.{ts,tsx}"], {
  query: "?raw",
  import: "default",
  eager: true,
});

// Blanks out comments, keeping line breaks so line numbers stay right.
// Strings are skipped over so "//" in a URL isn't taken for a comment.
function stripComments(code: string): string {
  let out = "";
  let i = 0;
  while (i < code.length) {
    const c = code[i];
    const next = code[i + 1];
    if (c === "/" && next === "/") {
      while (i < code.length && code[i] !== "\n") {
        out += " ";
        i++;
      }
    } else if (c === "/" && next === "*") {
      const end = code.indexOf("*/", i + 2);
      const stop = end === -1 ? code.length : end + 2;
      out += code.slice(i, stop).replace(/[^\n]/g, " ");
      i = stop;
    } else if (c === '"' || c === "'" || c === "`") {
      let j = i + 1;
      while (j < code.length && code[j] !== c) j += code[j] === "\\" ? 2 : 1;
      out += code.slice(i, j + 1);
      i = j + 1;
    } else {
      out += c;
      i++;
    }
  }
  return out;
}

const CYRILLIC = /[Ѐ-ӿ]/;

describe("UI strings", () => {
  it("scans the sources", () => {
    expect(Object.keys(sources).length).toBeGreaterThan(10);
    expect(Object.keys(sources)).not.toContain("./ru.ts");
  });

  it("keeps Russian text in the dictionary only", () => {
    const found: string[] = [];
    for (const [path, code] of Object.entries(sources)) {
      stripComments(code)
        .split("\n")
        .forEach((line, i) => {
          if (CYRILLIC.test(line)) found.push(`${path.replace(/^\.\.\//, "src/").replace(/^\.\//, "src/i18n/")}:${i + 1}: ${line.trim()}`);
        });
    }
    expect(found).toEqual([]);
  });
});
