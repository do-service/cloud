import { apiRequest } from "./api";
import { isDeviceList, type Device } from "./devices";
import { expectShape, isArrayOf, isBoolean, isNumber, isRecord, isString } from "./guards";

// The administrator's API (R15, E9-11): accounts, groups, the audit log,
// the system's settings.

export type AccountStatus = "active" | "disabled" | "deleting";

// Mirrors backend/internal/admin accountJSON.
export type Account = {
  id: string;
  username: string;
  displayName: string;
  email: string;
  isAdmin: boolean;
  status: AccountStatus;
  // null: no limit.
  quotaBytes: number | null;
  usedBytes: number;
  hasPassword: boolean;
  providers: string[];
  createdAt: string;
  lastSeenAt: string | null;
};

export type AccountPage = { items: Account[]; nextCursor: string | null };

export type NewAccount = { username: string; displayName: string; password: string; isAdmin: boolean; quotaBytes: number | null };

// What PATCH changes: only the fields present.
export type AccountChange = Partial<{
  displayName: string;
  isAdmin: boolean;
  status: "active" | "disabled";
  quotaBytes: number | null;
  password: string;
}>;

export type GroupMember = { type: string; id: string; name: string };
export type AdminGroup = { id: string; name: string; members: GroupMember[] };

export type AuditEntry = {
  id: number;
  at: string;
  actor: { id: string; name: string };
  ip: string;
  action: string;
  target: { id: string; name: string };
  details: Record<string, unknown>;
};
export type AuditPage = { items: AuditEntry[]; total: number; page: number; size: number };

// What GET /admin/audit narrows by; empty strings don't narrow. from and to
// are RFC 3339 times (to exclusive), actor a login, q part of the
// object's name or its id, ip an address or a network.
export type AuditQuery = {
  page: number;
  size: number;
  action: string;
  actor: string;
  from: string;
  to: string;
  q: string;
  ip: string;
};

export type SystemInfo = {
  version: string;
  passwordLogin: boolean;
  providers: { slug: string; name: string; issuer: string }[];
  editor: { configured: boolean; available: boolean; url: string };
  defaultQuotaBytes: number | null;
  auditRetentionDays: number;
  trashRetentionDays: number;
};

export const accountsQueryKey = ["admin", "users"] as const;
export const groupsQueryKey = ["admin", "groups"] as const;
export const auditQueryKey = ["admin", "audit"] as const;
export const systemQueryKey = ["admin", "system"] as const;
export const userDevicesQueryKey = (userId: string) => ["admin", "devices", userId] as const;

const nullableNumber = (v: unknown): v is number | null => v === null || isNumber(v);
const nullableString = (v: unknown): v is string | null => v === null || isString(v);

function isStatus(v: unknown): v is AccountStatus {
  return v === "active" || v === "disabled" || v === "deleting";
}

export function isAccount(v: unknown): v is Account {
  return (
    isRecord(v) &&
    isString(v.id) &&
    isString(v.username) &&
    isString(v.displayName) &&
    isString(v.email) &&
    isBoolean(v.isAdmin) &&
    isStatus(v.status) &&
    nullableNumber(v.quotaBytes) &&
    isNumber(v.usedBytes) &&
    isBoolean(v.hasPassword) &&
    isArrayOf(v.providers, isString) &&
    isString(v.createdAt) &&
    nullableString(v.lastSeenAt)
  );
}

function isAccountPage(v: unknown): v is AccountPage {
  return isRecord(v) && isArrayOf(v.items, isAccount) && nullableString(v.nextCursor);
}

function isMember(v: unknown): v is GroupMember {
  return isRecord(v) && isString(v.type) && isString(v.id) && isString(v.name);
}

function isGroup(v: unknown): v is AdminGroup {
  return isRecord(v) && isString(v.id) && isString(v.name) && isArrayOf(v.members, isMember);
}

function isNamed(v: unknown): v is { id: string; name: string } {
  return isRecord(v) && isString(v.id) && isString(v.name);
}

function isAuditEntry(v: unknown): v is AuditEntry {
  return (
    isRecord(v) &&
    isNumber(v.id) &&
    isString(v.at) &&
    isNamed(v.actor) &&
    isString(v.ip) &&
    isString(v.action) &&
    isNamed(v.target) &&
    isRecord(v.details)
  );
}

function isAuditPage(v: unknown): v is AuditPage {
  return isRecord(v) && isArrayOf(v.items, isAuditEntry) && isNumber(v.total) && isNumber(v.page) && isNumber(v.size);
}

function isProvider(v: unknown): v is SystemInfo["providers"][number] {
  return isRecord(v) && isString(v.slug) && isString(v.name) && isString(v.issuer);
}

function isSystem(v: unknown): v is SystemInfo {
  return (
    isRecord(v) &&
    isString(v.version) &&
    isBoolean(v.passwordLogin) &&
    isArrayOf(v.providers, isProvider) &&
    isRecord(v.editor) &&
    isBoolean(v.editor.configured) &&
    isBoolean(v.editor.available) &&
    isString(v.editor.url) &&
    nullableNumber(v.defaultQuotaBytes) &&
    isNumber(v.auditRetentionDays) &&
    isNumber(v.trashRetentionDays)
  );
}

const enc = encodeURIComponent;

export async function fetchAccounts(query: string, cursor: string | null, signal?: AbortSignal): Promise<AccountPage> {
  const params = new URLSearchParams({ q: query });
  if (cursor) params.set("cursor", cursor);
  return expectShape(await apiRequest(`/admin/users?${params}`, { signal }), isAccountPage, "GET /admin/users");
}

export async function createAccount(a: NewAccount): Promise<Account> {
  return expectShape(await apiRequest("/admin/users", { method: "POST", body: a }), isAccount, "POST /admin/users");
}

export async function updateAccount(id: string, change: AccountChange): Promise<Account> {
  return expectShape(await apiRequest(`/admin/users/${enc(id)}`, { method: "PATCH", body: change }), isAccount, "PATCH /admin/users/{id}");
}

export async function deleteAccount(id: string): Promise<void> {
  await apiRequest(`/admin/users/${enc(id)}`, { method: "DELETE" });
}

export async function fetchUserDevices(userId: string, signal?: AbortSignal): Promise<Device[]> {
  return expectShape(await apiRequest(`/admin/users/${enc(userId)}/devices`, { signal }), isDeviceList, "GET /admin/users/{id}/devices");
}

export async function revokeUserDevice(userId: string, deviceId: string): Promise<void> {
  await apiRequest(`/admin/users/${enc(userId)}/devices/${enc(deviceId)}`, { method: "DELETE" });
}

export async function fetchGroups(signal?: AbortSignal): Promise<AdminGroup[]> {
  const res = await apiRequest("/admin/groups", { signal });
  return expectShape(res, (v): v is { items: AdminGroup[] } => isRecord(v) && isArrayOf(v.items, isGroup), "GET /admin/groups").items;
}

export async function createGroup(name: string): Promise<void> {
  await apiRequest("/admin/groups", { method: "POST", body: { name } });
}

export async function renameGroup(id: string, name: string): Promise<void> {
  await apiRequest(`/admin/groups/${enc(id)}`, { method: "PATCH", body: { name } });
}

export async function deleteGroup(id: string): Promise<void> {
  await apiRequest(`/admin/groups/${enc(id)}`, { method: "DELETE" });
}

export async function setMember(groupId: string, userId: string, member: boolean): Promise<void> {
  await apiRequest(`/admin/groups/${enc(groupId)}/members/${enc(userId)}`, { method: member ? "PUT" : "DELETE" });
}

export async function fetchAudit(query: AuditQuery, signal?: AbortSignal): Promise<AuditPage> {
  const params = new URLSearchParams({ page: String(query.page), size: String(query.size) });
  for (const key of ["action", "actor", "from", "to", "q", "ip"] as const) {
    if (query[key]) params.set(key, query[key]);
  }
  return expectShape(await apiRequest(`/admin/audit?${params}`, { signal }), isAuditPage, "GET /admin/audit");
}

export async function fetchSystem(signal?: AbortSignal): Promise<SystemInfo> {
  return expectShape(await apiRequest("/admin/system", { signal }), isSystem, "GET /admin/system");
}

const GIB = 1024 ** 3;

// Quotas are entered in gigabytes (binary, as storage counts them).
export function bytesToGiB(bytes: number): number {
  return Math.round((bytes / GIB) * 100) / 100;
}

export function gibToBytes(gib: number): number {
  return Math.round(gib * GIB);
}
