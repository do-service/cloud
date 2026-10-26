import { ApiError, errorText, toApiError } from "@web/api";
import { expectShape, isArrayOf, isNumber, isRecord, isString } from "@web/guards";
import { t } from "@web/i18n";

// The app's own API (/app/v1, desktop/internal/app/handler.go): the
// connections and their engines, next to the server's /api, which the app
// proxies with the connection's device token.

export const STATUSES = ["starting", "synced", "syncing", "paused", "offline", "error", "needs_signin", "no_folder", "busy", "moving"] as const;
export type Status = (typeof STATUSES)[number];

export type Connection = {
  id: string;
  server: string;
  host: string;
  user: string;
  folder: string;
  status: Status;
  pending: number;
  problems: number;
};

export type Connections = { items: Connection[]; active: string };

export const SIGNIN_ERRORS = [
  "invalid_server",
  "not_a_cloud",
  "client_too_old",
  "unreachable",
  "denied",
  "timeout",
  "busy",
  "cancelled",
  "other_folder",
] as const;
export type SignInError = (typeof SIGNIN_ERRORS)[number];

export type SignIn = {
  state: "idle" | "waiting" | "done" | "error";
  server?: string;
  url?: string;
  code?: string;
  connId?: string;
};

// Why a folder can't be a sync folder (internal/app folderCode).
export const FOLDER_ERRORS = [
  "nested",
  "not_a_folder",
  "folder_not_empty",
  "folder_taken",
  "folder_inside",
  "cross_volume",
  "no_folder",
  "unsupported",
] as const;

// A folder as the app checked it (connection.Folder); code says why it
// can't be used.
export type FolderCheck = {
  path: string;
  exists: boolean;
  files: number;
  folders: number;
  more: boolean;
  replace: boolean;
  code?: string;
};

export type Check = "off" | "partial" | "on";
export type SyncState = "" | "synced" | "pending" | "error";
export type Mark = { check: Check; sync: SyncState };

export type Usage = { selected: number; free: number };

function isStatus(v: unknown): v is Status {
  return STATUSES.some((s) => s === v);
}

function isConnection(v: unknown): v is Connection {
  return (
    isRecord(v) &&
    isString(v.id) &&
    isString(v.server) &&
    isString(v.host) &&
    isString(v.user) &&
    isString(v.folder) &&
    isStatus(v.status) &&
    isNumber(v.pending) &&
    isNumber(v.problems)
  );
}

function isConnections(v: unknown): v is Connections {
  return isRecord(v) && isArrayOf(v.items, isConnection) && isString(v.active);
}

function isSignIn(v: unknown): v is SignIn {
  return (
    isRecord(v) &&
    (v.state === "idle" || v.state === "waiting" || v.state === "done" || v.state === "error") &&
    (v.url === undefined || isString(v.url)) &&
    (v.code === undefined || isString(v.code)) &&
    (v.connId === undefined || isString(v.connId))
  );
}

function isFolderCheck(v: unknown): v is FolderCheck {
  return (
    isRecord(v) &&
    isString(v.path) &&
    typeof v.exists === "boolean" &&
    isNumber(v.files) &&
    isNumber(v.folders) &&
    typeof v.more === "boolean" &&
    typeof v.replace === "boolean" &&
    (v.code === undefined || isString(v.code))
  );
}

function isPath(v: unknown): v is { path: string } {
  return isRecord(v) && isString(v.path);
}

function isMoved(v: unknown): v is { folder: string } {
  return isRecord(v) && isString(v.folder);
}

function isIDs(v: unknown): v is { items: string[] } {
  return isRecord(v) && isArrayOf(v.items, isString);
}

function isMark(v: unknown): v is Mark {
  return (
    isRecord(v) &&
    (v.check === "off" || v.check === "partial" || v.check === "on") &&
    (v.sync === "" || v.sync === "synced" || v.sync === "pending" || v.sync === "error")
  );
}

function isMarks(v: unknown): v is { items: Record<string, Mark> } {
  return isRecord(v) && isRecord(v.items) && Object.values(v.items).every(isMark);
}

function isUsage(v: unknown): v is Usage {
  return isRecord(v) && isNumber(v.selected) && isNumber(v.free);
}

// NoSpaceError: checking would download more than the disk has room for.
export class NoSpaceError extends ApiError {
  readonly need: number;
  constructor(need: number) {
    super(507, "no_space", "not enough free disk space");
    this.need = need;
  }
}

async function call(method: string, path: string, body?: unknown): Promise<unknown> {
  const url = `/app/v1${path}`;
  let res: Response;
  try {
    res = await fetch(url, {
      method,
      headers: { Accept: "application/json", ...(body !== undefined ? { "Content-Type": "application/json" } : {}) },
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    });
  } catch (err) {
    throw new ApiError(0, "network", `${method} ${url}: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (res.status === 507) {
    const raw: unknown = await res.json().catch(() => undefined);
    throw new NoSpaceError(isRecord(raw) && isNumber(raw.need) ? raw.need : 0);
  }
  if (!res.ok) throw await toApiError(res, `${method} ${url}`);
  if (res.status === 204) return undefined;
  return res.json();
}

const conn = (id: string) => `/connections/${encodeURIComponent(id)}`;

export const connectionsQueryKey = ["app", "connections"] as const;

export async function fetchConnections(): Promise<Connections> {
  return expectShape(await call("GET", "/connections"), isConnections, "GET /app/v1/connections");
}

export async function setActive(id: string): Promise<void> {
  await call("PUT", "/active", { id });
}

export async function removeConnection(id: string, force: boolean): Promise<void> {
  await call("DELETE", `${conn(id)}${force ? "?force=1" : ""}`);
}

export async function openFolder(id: string): Promise<void> {
  await call("POST", `${conn(id)}/open-folder`);
}

export async function setPaused(id: string, paused: boolean): Promise<void> {
  await call("POST", `${conn(id)}/${paused ? "pause" : "resume"}`);
}

// folder "" is the default one (newFolder tells which).
export async function startSignIn(server: string, folder: string): Promise<SignIn> {
  return expectShape(await call("POST", "/signin", { server, folder }), isSignIn, "POST /app/v1/signin");
}

// newFolder checks the folder «Добавить облако» would connect: path, or
// the default for server when "".
export async function newFolder(server: string, path: string): Promise<FolderCheck> {
  const q = new URLSearchParams({ server, path });
  return expectShape(await call("GET", `/new-folder?${q.toString()}`), isFolderCheck, "GET /app/v1/new-folder");
}

// pickFolder opens the OS's folder dialog; "" when cancelled.
export async function pickFolder(start: string): Promise<string> {
  return expectShape(await call("POST", "/pick-folder", { start }), isPath, "POST /app/v1/pick-folder").path;
}

export async function checkMoveFolder(connId: string, path: string): Promise<FolderCheck> {
  const q = new URLSearchParams({ path });
  return expectShape(await call("GET", `${conn(connId)}/folder?${q.toString()}`), isFolderCheck, "GET /app/v1/connections/{id}/folder");
}

export async function moveFolder(connId: string, path: string): Promise<string> {
  return expectShape(await call("PUT", `${conn(connId)}/folder`, { path }), isMoved, "PUT /app/v1/connections/{id}/folder").folder;
}

export function adoptableQueryKey(connId: string) {
  return ["app", "adoptable", connId] as const;
}

export async function fetchAdoptable(connId: string): Promise<string[]> {
  return expectShape(await call("GET", `${conn(connId)}/adoptable`), isIDs, "GET /app/v1/connections/{id}/adoptable").items;
}

export async function adoptExisting(connId: string): Promise<void> {
  await call("POST", `${conn(connId)}/adopt`);
}

export async function fetchSignIn(): Promise<SignIn> {
  return expectShape(await call("GET", "/signin"), isSignIn, "GET /app/v1/signin");
}

export async function cancelSignIn(): Promise<void> {
  await call("DELETE", "/signin");
}

export function marksQueryKey(connId: string, ids: string[]) {
  return ["app", "marks", connId, ids] as const;
}

export async function fetchMarks(connId: string, ids: string[]): Promise<Record<string, Mark>> {
  if (ids.length === 0) return {};
  return expectShape(await call("POST", `${conn(connId)}/marks`, { ids }), isMarks, "POST /app/v1/connections/{id}/marks").items;
}

export function usageQueryKey(connId: string) {
  return ["app", "usage", connId] as const;
}

export async function fetchUsage(connId: string): Promise<Usage> {
  return expectShape(await call("GET", `${conn(connId)}/usage`), isUsage, "GET /app/v1/connections/{id}/usage");
}

export async function setChecked(connId: string, nodeId: string, on: boolean): Promise<void> {
  await call(on ? "PUT" : "DELETE", `${conn(connId)}/selection/${encodeURIComponent(nodeId)}`);
}

const APP_ERRORS = ["not_ready", "busy", "no_space", "not_found"] as const;

// appErrorText is errorText with the app's own codes.
export function appErrorText(err: unknown): string {
  if (err instanceof ApiError) {
    const folder = folderErrorText(err.code);
    if (folder) return folder;
    const code = APP_ERRORS.find((c) => c === err.code);
    if (code) return t(`desktop.errors.${code}`);
  }
  return errorText(err);
}

export function folderErrorText(code: string): string | null {
  const known = FOLDER_ERRORS.find((c) => c === code);
  return known ? t(`desktop.folderErrors.${known}`) : null;
}

export function signInErrorText(code: string | undefined): string {
  const folder = code === undefined ? null : folderErrorText(code);
  if (folder) return folder;
  const known = SIGNIN_ERRORS.find((c) => c === code) ?? "unreachable";
  return t(`desktop.signin.errors.${known}`);
}
