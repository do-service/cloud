import { apiRequest } from "./api";
import { expectShape, isArrayOf, isNumber, isRecord, isString } from "./guards";

// The account's own settings (E9-10): quota, app passwords, WebDAV.

// Mirrors backend/internal/nodes quotaResponse; quotaBytes null is no limit.
export type Quota = { usedBytes: number; quotaBytes: number | null };

// Mirrors backend/internal/auth AppPassword.
export type AppPassword = { id: string; name: string; createdAt: string; lastUsedAt: string | null };

export const quotaQueryKey = ["quota"] as const;
export const appPasswordsQueryKey = ["app-passwords"] as const;

function isQuota(v: unknown): v is Quota {
  return isRecord(v) && isNumber(v.usedBytes) && (v.quotaBytes === null || isNumber(v.quotaBytes));
}

function isAppPassword(v: unknown): v is AppPassword {
  return isRecord(v) && isString(v.id) && isString(v.name) && isString(v.createdAt) && (v.lastUsedAt === null || isString(v.lastUsedAt));
}

function isCreated(v: unknown): v is AppPassword & { password: string } {
  return isRecord(v) && isString(v.password) && isAppPassword(v);
}

export async function fetchQuota(signal?: AbortSignal): Promise<Quota> {
  return expectShape(await apiRequest("/me/quota", { signal }), isQuota, "GET /me/quota");
}

export async function fetchAppPasswords(signal?: AbortSignal): Promise<AppPassword[]> {
  const res = await apiRequest("/me/app-passwords", { signal });
  return expectShape(res, (v): v is AppPassword[] => isArrayOf(v, isAppPassword), "GET /me/app-passwords");
}

// createAppPassword returns the password itself: the only time it is shown.
export async function createAppPassword(name: string): Promise<string> {
  const res = await apiRequest("/me/app-passwords", { method: "POST", body: { name } });
  return expectShape(res, isCreated, "POST /me/app-passwords").password;
}

export async function deleteAppPassword(id: string): Promise<void> {
  await apiRequest(`/me/app-passwords/${encodeURIComponent(id)}`, { method: "DELETE" });
}

// The address WebDAV clients connect to: the user's own tree.
export function webdavUrl(username: string): string {
  return `${window.location.origin}/dav/files/${encodeURIComponent(username)}/`;
}
