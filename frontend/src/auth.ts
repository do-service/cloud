import { apiRequest } from "./api";
import { expectShape, isArrayOf, isBoolean, isRecord, isString } from "./guards";

// Mirrors backend/internal/auth's User JSON.
export type User = {
  id: string;
  username: string;
  displayName: string;
  isAdmin: boolean;
};

export type Provider = { slug: string; name: string };

// What the login page offers: the password form and/or provider buttons.
export type SignInOptions = { password: boolean; providers: Provider[] };

export const meQueryKey = ["me"] as const;

export function isUser(v: unknown): v is User {
  return isRecord(v) && isString(v.id) && isString(v.username) && isString(v.displayName) && isBoolean(v.isAdmin);
}

function isProvider(v: unknown): v is Provider {
  return isRecord(v) && isString(v.slug) && isString(v.name);
}

function isSignInOptions(v: unknown): v is SignInOptions {
  return isRecord(v) && isBoolean(v.password) && isArrayOf(v.providers, isProvider);
}

export async function fetchMe(signal?: AbortSignal): Promise<User> {
  return expectShape(await apiRequest("/auth/me", { signal }), isUser, "GET /auth/me");
}

export async function fetchSignInOptions(signal?: AbortSignal): Promise<SignInOptions> {
  return expectShape(await apiRequest("/auth/providers", { signal }), isSignInOptions, "GET /auth/providers");
}

export async function login(username: string, password: string): Promise<User> {
  const res = await apiRequest("/auth/login", { method: "POST", body: { username, password } });
  return expectShape(res, isUser, "POST /auth/login");
}

export async function logout(): Promise<void> {
  await apiRequest("/auth/logout", { method: "POST" });
}

// Where a provider button goes: a full-page navigation into the OIDC flow,
// which comes back to `next` once signed in.
export function providerStartUrl(slug: string, next: string): string {
  return `/api/v1/auth/oidc/${encodeURIComponent(slug)}/start?next=${encodeURIComponent(next)}`;
}

// A return path the login page accepts from ?next=: same-origin paths
// only, so a crafted link can't bounce the user elsewhere after sign-in.
// The backend applies the same rule to the OIDC flow.
export function safeNext(next: string | null): string {
  if (!next || !next.startsWith("/") || next.startsWith("//") || next.startsWith("/\\")) return "/";
  return next;
}
