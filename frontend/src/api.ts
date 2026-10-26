import { t } from "./i18n";

const API_BASE = "/api/v1";

// Backend error codes the UI has a translation for. Each must exist under
// `errors.*` in the dictionary — errorText's template-literal key below makes
// a missing one a compile error. Unlisted codes still work, they just show
// the generic errors.unknown text.
// Mirrors backend/internal/httpx's Code* constants; `unknown` and `network`
// are client-side only.
export const ERROR_CODES = [
  "unknown",
  "network",
  "internal_error",
  "not_found",
  "database_unavailable",
  "data_dir_unavailable",
  "request_body_too_large",
  "unauthorized",
  "cross_origin_request",
  "invalid_request",
  "invalid_credentials",
  "too_many_requests",
  "provider_unavailable",
  "oidc_failed",
  "signup_disabled",
  "account_disabled",
  "app_password_limit",
  "device_limit",
  "device_forbidden",
  "invalid_grant",
  "not_a_directory",
  "not_a_file",
  "name_conflict",
  "invalid_name",
  "move_into_itself",
  "root_immutable",
  "query_too_short",
  "quota_exceeded",
  "checksum_mismatch",
  "forbidden",
  "cross_owner_move",
  "invalid_grantee",
  "invalid_permissions",
  "links_disabled",
  "locked",
  "editor_unavailable",
  "not_editable",
  "username_taken",
  "invalid_username",
  "weak_password",
  "last_admin",
  "own_account",
  "cursor_expired",
  "precondition_failed",
] as const;

export type ErrorCode = (typeof ERROR_CODES)[number];

function isKnownErrorCode(code: string): code is ErrorCode {
  return ERROR_CODES.some((known) => known === code);
}

// Every failed request surfaces as this, whatever went wrong. `code` is the
// stable machine-readable value from the backend's `{"code", "message"}`
// error body and is what the UI branches on; `message` is the backend's
// English developer hint, for logs only — never shown to the user (use
// errorText for that). status 0 means the request never got a response.
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

export async function toApiError(res: Response, label: string): Promise<ApiError> {
  const fallback = `${label} failed: ${res.status}`;
  const text = await res.text().catch(() => "");
  let body: unknown;
  try {
    body = JSON.parse(text);
  } catch {
    // A proxy's HTML error page, an empty body, etc. — not our contract.
    return new ApiError(res.status, "unknown", fallback);
  }
  if (!isRecord(body) || typeof body.code !== "string") {
    return new ApiError(res.status, "unknown", fallback);
  }
  const message = typeof body.message === "string" ? body.message : fallback;
  return new ApiError(res.status, body.code, message);
}

export type RequestOptions = {
  method?: string;
  // Serialized as JSON; omitted entirely for bodiless requests.
  body?: unknown;
  signal?: AbortSignal;
  // Extra request headers, e.g. a sync client's If-Match.
  headers?: Record<string, string>;
};

// Low-level fetch for an absolute same-origin URL. Resolves with the parsed
// JSON body, or undefined when the response isn't JSON (204, a plain-text
// probe reply). The result is `unknown` on purpose: callers narrow it with a
// type guard rather than trusting the wire format via a cast.
async function send(url: string, options: RequestOptions = {}): Promise<unknown> {
  const { method = "GET", body, signal, headers } = options;
  const label = `${method} ${url}`;
  let res: Response;
  try {
    res = await fetch(url, {
      method,
      signal,
      // Auth will ride a session cookie; same-origin is fetch's default for
      // a relative URL, stated explicitly so it can't change silently.
      credentials: "same-origin",
      headers: {
        Accept: "application/json",
        ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
        ...headers,
      },
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    });
  } catch (err) {
    // Cancellation (e.g. TanStack Query dropping a stale request) isn't a
    // failure the user should hear about — let it propagate unchanged.
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    throw new ApiError(0, "network", `${label}: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (!res.ok) throw await toApiError(res, label);
  const contentType = res.headers.get("Content-Type") ?? "";
  if (res.status === 204 || !contentType.includes("application/json")) return undefined;
  return res.json();
}

// Request against the versioned JSON API: `path` is relative to /api/v1.
export function apiRequest(path: string, options?: RequestOptions): Promise<unknown> {
  return send(`${API_BASE}${path}`, options);
}

// Request against a public link's API: `path` is relative to
// /s/{token}/api. Used without an account; the token is the key.
export function linkRequest(token: string, path: string, options?: RequestOptions): Promise<unknown> {
  return send(`/s/${encodeURIComponent(token)}/api${path}`, options);
}

// Readiness probe. Lives at the root rather than under /api/v1 because it is
// also hit by orchestrators; resolves when the backend can serve requests,
// rejects with an ApiError (typically database_unavailable or network) otherwise.
export async function checkReady(signal?: AbortSignal): Promise<void> {
  await send("/readyz", { signal });
}

// User-facing text for any thrown error. Non-ApiError values (bugs, thrown
// strings) get the generic message too, so callers never need to branch.
export function errorText(err: unknown): string {
  if (err instanceof ApiError && isKnownErrorCode(err.code)) {
    return t(`errors.${err.code}`);
  }
  return t("errors.unknown");
}
