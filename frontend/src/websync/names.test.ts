import { describe, expect, it } from "vitest";
import { isArrayOf, isBoolean, isRecord, isString } from "../guards";
import { conflictName, conflictStamp, isIgnored, localNameAllowed, nameKey, pathKey, validName } from "./names";

type Vector = { note: string; in?: string; inHex?: string; valid: boolean; name?: string; key?: string };

const opt = (v: unknown) => v === undefined || isString(v);

function isVector(v: unknown): v is Vector {
  return isRecord(v) && isString(v.note) && isBoolean(v.valid) && opt(v.in) && opt(v.inHex) && opt(v.name) && opt(v.key);
}

// Read through Vite rather than imported: the frontend's Docker build
// type-checks without the backend's tree next to it.
const files = import.meta.glob<unknown>("../../../backend/internal/fs/testdata/name_vectors.json", { import: "default", eager: true });
const raw = Object.values(files)[0];
const vectors: Vector[] = isArrayOf(raw, isVector) ? raw : [];

describe("nameKey", () => {
  // The server's golden vectors (backend/internal/fs/names_vectors_test.go):
  // a key computed here must be the server's, or the two sides disagree
  // about which names are one.
  const cases = vectors;
  it("has the server's vectors", () => {
    expect(cases.length).toBeGreaterThan(40);
  });
  it.each(cases.filter((v) => v.inHex === undefined).map((v): [string, Vector] => [v.note, v]))("%s", (_, v) => {
    const input = v.in ?? "";
    expect(validName(input)).toBe(v.valid);
    if (v.valid) {
      expect(input.normalize("NFC")).toBe(v.name);
      expect(nameKey(input)).toBe(v.key);
    }
  });

  it("keys a path component by component", () => {
    expect(pathKey("Доступно/ОТЧЁТ.docx")).toBe("доступно/отчёт.docx");
    expect(pathKey("")).toBe("");
  });

  it("refuses a lone surrogate, which isn't UTF-8", () => {
    expect(validName("a\ud800b")).toBe(false);
    expect(validName("😀")).toBe(true);
  });
});

describe("localNameAllowed", () => {
  it.each([
    ["report.txt", false, true],
    ["a\\b", false, false],
    ['win<>:"|?*', false, true],
    ['win<>:"|?*', true, false],
    ["name.", true, false],
    ["name ", true, false],
    ["CON", true, false],
    ["nul.txt", true, false],
    ["COM1.tar.gz", true, false],
    ["COM10", true, true],
    ["console", true, true],
  ])("%j on windows=%s → %s", (name, windows, want) => {
    expect(localNameAllowed(name, windows)).toBe(want);
  });
});

describe("isIgnored", () => {
  it.each([
    [".DS_Store", true],
    ["Thumbs.db", true],
    ["desktop.ini", true],
    ["~$doc.docx", true],
    [".~lock.doc.odt#", true],
    ["a.TMP", true],
    ["video.crdownload", true],
    ["file.part", true],
    ["notes.txt.crswap", true],
    [".~cloud-123.part", true],
    ["report.txt", false],
    [".hidden", false],
  ])("%j → %s", (name, want) => {
    expect(isIgnored(name)).toBe(want);
  });
});

describe("conflictName", () => {
  const tag = "(conflict, Browser, 2026-10-26 09-05)";

  it("puts the tag before the last extension", () => {
    expect(conflictName("report.final.docx", tag)).toBe(`report.final ${tag}.docx`);
    expect(conflictName("README", tag)).toBe(`README ${tag}`);
    expect(conflictName(".env", tag)).toBe(`.env ${tag}`);
  });

  it("stays within 255 bytes", () => {
    const name = `${"я".repeat(120)}.txt`;
    const out = conflictName(name, tag);
    expect(new TextEncoder().encode(out).length).toBeLessThanOrEqual(255);
    expect(out.endsWith(`${tag}.txt`)).toBe(true);
    expect(validName(out)).toBe(true);
  });

  it("dates in local time", () => {
    expect(conflictStamp(new Date(2026, 0, 2, 3, 4))).toBe("2026-01-02 03-04");
  });
});
