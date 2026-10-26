// Minimal i18n layer. The UI ships Russian-only for now, but every
// user-visible string already goes through t() so adding a language later
// touches only this folder:
//   1. add `xx.ts` exporting a dictionary typed as `Messages`
//      (the compiler then flags any missing or extra key);
//   2. add it to `dictionaries` and widen `Locale`;
//   3. pick `locale` (from settings, navigator.language, ...).
// No component changes needed. A library (i18next, FormatJS) can replace
// this once plurals or runtime switching are actually required.
import { ru } from "./ru";

// Same tree as the reference dictionary with every leaf widened to string,
// so another locale must match the structure but not the Russian literals.
type DeepStrings<T> = {
  readonly [K in keyof T]: T[K] extends string ? string : DeepStrings<T[K]>;
};

export type Messages = DeepStrings<typeof ru>;

// Every dotted path that ends at a string leaf, e.g. "errors.network".
// Derived from the dictionary type, so t("typo.key") is a compile error.
type LeafPaths<T> = {
  [K in keyof T & string]: T[K] extends string ? K : `${K}.${LeafPaths<T[K]>}`;
}[keyof T & string];

export type MessageKey = LeafPaths<Messages>;

export type Locale = "ru";

const dictionaries: Record<Locale, Messages> = { ru };

export const locale: Locale = "ru";

const messages: Messages = dictionaries[locale];

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function lookup(key: string): string | undefined {
  let node: unknown = messages;
  for (const part of key.split(".")) {
    node = isRecord(node) ? node[part] : undefined;
  }
  return typeof node === "string" ? node : undefined;
}

export type MessageParams = Record<string, string | number>;

// Translates `key`, substituting `{name}` placeholders from `params`.
// A placeholder without a matching param is left as-is so the gap is
// visible in the UI instead of silently rendering an empty string.
export function t(key: MessageKey, params?: MessageParams): string {
  // Unreachable for a well-typed key; the key itself is the most useful
  // fallback if a dictionary is ever out of sync at runtime.
  const template = lookup(key) ?? key;
  if (!params) return template;
  return template.replace(/\{(\w+)\}/g, (match, name: string) => {
    const value = params[name];
    return value === undefined ? match : String(value);
  });
}

export function formatNumber(value: number, options?: Intl.NumberFormatOptions): string {
  return new Intl.NumberFormat(locale, options).format(value);
}

export function formatDate(
  value: Date | number | string,
  options: Intl.DateTimeFormatOptions = { dateStyle: "medium", timeStyle: "short" },
): string {
  const date = value instanceof Date ? value : new Date(value);
  return new Intl.DateTimeFormat(locale, options).format(date);
}

const BYTE_UNITS = [
  "units.bytes.b",
  "units.bytes.kb",
  "units.bytes.mb",
  "units.bytes.gb",
  "units.bytes.tb",
] as const satisfies readonly MessageKey[];

// Human-readable size with binary (1024) steps — what file managers show
// and what matches storage quotas. Caps at ТБ: larger values stay in ТБ
// with a bigger number rather than inventing units nobody recognises.
export function formatBytes(bytes: number): string {
  let value = bytes;
  let unit = 0;
  while (Math.abs(value) >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024;
    unit++;
  }
  // Whole bytes never have a fraction; larger units keep one digit so
  // 1.5 GiB doesn't collapse to "2 ГБ".
  const number = formatNumber(value, { maximumFractionDigits: unit === 0 ? 0 : 1 });
  // Non-breaking space keeps "1,5 ГБ" from wrapping between number and unit.
  return `${number}\u00A0${t(BYTE_UNITS[unit])}`;
}
