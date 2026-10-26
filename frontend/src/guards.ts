// Narrowing helpers for API responses. The wire format is `unknown` until a
// guard says otherwise (see api.ts), so a backend change shows up as a
// clear "unexpected response" instead of undefined fields deep in the UI.

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

export function isString(value: unknown): value is string {
  return typeof value === "string";
}

export function isNumber(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value);
}

export function isBoolean(value: unknown): value is boolean {
  return typeof value === "boolean";
}

export function isArrayOf<T>(value: unknown, item: (v: unknown) => v is T): value is T[] {
  return Array.isArray(value) && value.every(item);
}

// Thrown when a 2xx response doesn't have the shape the UI expects.
export class UnexpectedResponseError extends Error {
  constructor(what: string) {
    super(`unexpected response: ${what}`);
    this.name = "UnexpectedResponseError";
  }
}

export function expectShape<T>(value: unknown, guard: (v: unknown) => v is T, what: string): T {
  if (!guard(value)) throw new UnexpectedResponseError(what);
  return value;
}
