import { apiRequest } from "./api";
import { expectShape, isArrayOf, isRecord, isString } from "./guards";

// The desktop sign-in (E12-2): the app opens /devices/authorize in the
// system browser, the user consents, and the page sends the browser back
// to the app's loopback address with a one-time code.

export const DEVICE_PLATFORMS = ["macos", "windows", "linux"] as const;

export type DevicePlatform = (typeof DEVICE_PLATFORMS)[number];

// What the app asks for, from the page's query; mirrors the body of
// POST /api/v1/devices/authorize.
export type DeviceRequest = {
  redirectUri: string;
  codeChallenge: string;
  codeChallengeMethod: "S256";
  state: string;
  name: string;
  platform: DevicePlatform;
  clientVersion: string;
};

function isPlatform(v: unknown): v is DevicePlatform {
  return DEVICE_PLATFORMS.some((p) => p === v);
}

// A signed-in desktop client (E12-3); mirrors backend/internal/auth Device.
export type Device = {
  id: string;
  name: string;
  platform: DevicePlatform;
  clientVersion: string;
  createdAt: string;
  lastUsedAt: string | null;
  lastIp: string | null;
};

export const devicesQueryKey = ["devices"] as const;

const nullableString = (v: unknown): v is string | null => v === null || isString(v);

export function isDevice(v: unknown): v is Device {
  return (
    isRecord(v) &&
    isString(v.id) &&
    isString(v.name) &&
    isPlatform(v.platform) &&
    isString(v.clientVersion) &&
    isString(v.createdAt) &&
    nullableString(v.lastUsedAt) &&
    nullableString(v.lastIp)
  );
}

export const isDeviceList = (v: unknown): v is Device[] => isArrayOf(v, isDevice);

export async function fetchDevices(signal?: AbortSignal): Promise<Device[]> {
  return expectShape(await apiRequest("/me/devices", { signal }), isDeviceList, "GET /me/devices");
}

// The token stops working with the device's next request.
export async function revokeDevice(id: string): Promise<void> {
  await apiRequest(`/me/devices/${encodeURIComponent(id)}`, { method: "DELETE" });
}

// Counted as the backend counts, in code points.
const length = (s: string) => [...s].length;

// The backend's rule (auth.checkRedirectURI): a loopback IP literal, a
// port from 1024, a path, nothing after it, and already in canonical form.
// Checked here too so a bad link shows an error instead of a consent.
export function isLoopbackRedirect(raw: string): boolean {
  const m = /^http:\/\/(?:127\.0\.0\.1|\[::1\]):([1-9][0-9]{3,4})\/[^?#\\]*$/.exec(raw);
  if (!m) return false;
  const port = Number(m[1]);
  if (port < 1024 || port > 65535) return false;
  try {
    return new URL(raw).href === raw;
  } catch {
    return false;
  }
}

// The request in a page's query, or null when anything is off: then the
// page shows an error and never redirects, as the address can't be trusted.
export function parseDeviceRequest(params: URLSearchParams): DeviceRequest | null {
  const redirectUri = params.get("redirect_uri") ?? "";
  const codeChallenge = params.get("code_challenge") ?? "";
  const state = params.get("state") ?? "";
  const name = (params.get("name") ?? "").trim();
  const platform = params.get("platform") ?? "";
  const clientVersion = params.get("client_version") ?? "";
  if (!isLoopbackRedirect(redirectUri)) return null;
  if (params.get("code_challenge_method") !== "S256" || !/^[A-Za-z0-9_-]{43}$/.test(codeChallenge)) return null;
  if (length(state) < 1 || length(state) > 200) return null;
  if (length(name) < 1 || length(name) > 100 || length(clientVersion) > 64) return null;
  if (!isPlatform(platform)) return null;
  return { redirectUri, codeChallenge, codeChallengeMethod: "S256", state, name, platform, clientVersion };
}

function isRedirect(v: unknown): v is { redirect: string } {
  return isRecord(v) && isString(v.redirect);
}

// Consents (or, with deny, refuses) and returns where to send the
// browser: the app's address with the code, or with access_denied.
export async function authorizeDevice(req: DeviceRequest, deny: boolean): Promise<string> {
  const res = await apiRequest("/devices/authorize", { method: "POST", body: deny ? { ...req, deny: true } : req });
  return expectShape(res, isRedirect, "POST /devices/authorize").redirect;
}

// A full navigation out of the app, to the desktop client's listener; its
// own function so tests can see where the page went.
export function leaveTo(url: string): void {
  window.location.assign(url);
}
