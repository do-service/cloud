import { describe, it, expect } from "vitest";
import { ERROR_CODES } from "../api";
import { formatBytes, formatNumber, t } from ".";
import { ru } from "./ru";

// Intl's ru-RU output uses non-breaking spaces (group separator and our own
// number/unit gap); normalizing keeps the expectations readable.
const plain = (s: string) => s.replace(/[  ]/g, " ");

describe("t", () => {
  it("returns the dictionary string for a key", () => {
    expect(t("app.title")).toBe("Облако");
  });
  it("interpolates {name} placeholders, including numbers", () => {
    expect(t("notFound.description", { path: "/nope" })).toBe("Адрес /nope не существует или был перемещён.");
  });
  it("leaves a placeholder visible when its param is missing", () => {
    expect(t("notFound.description", {})).toContain("{path}");
  });
});

describe("errors dictionary", () => {
  it("has a translation for every error code api.ts knows", () => {
    for (const code of ERROR_CODES) {
      expect(ru.errors[code]).toEqual(expect.any(String));
      expect(t(`errors.${code}`)).not.toBe(`errors.${code}`);
    }
  });
});

describe("formatBytes", () => {
  it("formats sizes with binary steps and Russian units", () => {
    expect(plain(formatBytes(0))).toBe("0 Б");
    expect(plain(formatBytes(1023))).toBe("1 023 Б");
    expect(plain(formatBytes(1024))).toBe("1 КБ");
    expect(plain(formatBytes(1.5 * 1024 ** 3))).toBe("1,5 ГБ");
  });
  it("caps at ТБ for huge values", () => {
    expect(plain(formatBytes(1024 ** 5))).toBe(`${plain(formatNumber(1024))} ТБ`);
    expect(plain(formatBytes(1024 ** 6))).toBe("1 048 576 ТБ");
  });
  it("keeps the number and unit on one line", () => {
    expect(formatBytes(2048)).toBe("2 КБ");
  });
});
