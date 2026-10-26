import { apiRequest } from "./api";
import { expectShape, isArrayOf, isBoolean, isNumber, isRecord, isString } from "./guards";

// Mirrors backend/internal/nodes' JSON.
export type NodeKind = "dir" | "file";

export type FileNode = {
  id: string;
  // null for the root and for the top of what is shared with the user.
  // Optional: the UI doesn't use it, and fixtures may leave it out.
  parentId?: string | null;
  name: string;
  kind: NodeKind;
  // For a folder: the size of everything below it.
  size: number;
  mime?: string;
  etag: string;
  // Hex SHA-256 of a file's content; absent for a folder.
  sha256?: string;
  mtime: string;
  // The signed-in user's rights on it.
  permissions: Permission[];
  // Shared with someone or by a link, on the node itself; told only to
  // those who may manage its shares.
  shared?: boolean;
};

export type Permission = "read" | "write" | "reshare";

function isPermission(v: unknown): v is Permission {
  return v === "read" || v === "write" || v === "reshare";
}

// can reports whether the user may do p with n; a node not loaded yet
// allows nothing.
export function can(n: { permissions: Permission[] } | undefined, p: Permission): boolean {
  return n?.permissions.includes(p) ?? false;
}

export type PathEntry = { id: string; name: string };

// A node with the folders from the root down to it; the root's name is "".
export type NodeWithPath = FileNode & { path: PathEntry[] };

export type ChildrenPage = { items: FileNode[]; nextCursor: string | null };

export type SortKey = "name" | "mtime" | "size";
export type SortOrder = "asc" | "desc";
export type Sort = { key: SortKey; order: SortOrder };

export const SORT_KEYS: readonly SortKey[] = ["name", "mtime", "size"];

// The backend's alias for the signed-in user's root folder.
export const ROOT = "root";

// Big enough that a screenful never needs two requests, small enough to
// keep the first paint of a 50 000-item folder quick.
export const PAGE_SIZE = 200;

function isKind(v: unknown): v is NodeKind {
  return v === "dir" || v === "file";
}

export function isFileNode(v: unknown): v is FileNode {
  return (
    isRecord(v) &&
    isString(v.id) &&
    (v.parentId === undefined || v.parentId === null || isString(v.parentId)) &&
    isString(v.name) &&
    isKind(v.kind) &&
    isNumber(v.size) &&
    (v.mime === undefined || isString(v.mime)) &&
    isString(v.etag) &&
    (v.sha256 === undefined || isString(v.sha256)) &&
    isString(v.mtime) &&
    isArrayOf(v.permissions, isPermission) &&
    (v.shared === undefined || isBoolean(v.shared))
  );
}

function isPathEntry(v: unknown): v is PathEntry {
  return isRecord(v) && isString(v.id) && isString(v.name);
}

function isNodeWithPath(v: unknown): v is NodeWithPath {
  return isRecord(v) && isArrayOf(v.path, isPathEntry) && isFileNode(v);
}

function isChildrenPage(v: unknown): v is ChildrenPage {
  return isRecord(v) && isArrayOf(v.items, isFileNode) && (v.nextCursor === null || isString(v.nextCursor));
}

export function isSortKey(v: string | null): v is SortKey {
  return v === "name" || v === "mtime" || v === "size";
}

export function nodeQueryKey(id: string) {
  return ["node", id] as const;
}

export function childrenQueryKey(id: string, sort: Sort) {
  return ["children", id, sort.key, sort.order] as const;
}

export async function fetchNode(id: string, signal?: AbortSignal): Promise<NodeWithPath> {
  const res = await apiRequest(`/nodes/${encodeURIComponent(id)}`, { signal });
  return expectShape(res, isNodeWithPath, "GET /nodes/{id}");
}

export async function fetchChildren(id: string, sort: Sort, cursor: string | null, signal?: AbortSignal): Promise<ChildrenPage> {
  const params = new URLSearchParams({ sort: sort.key, order: sort.order, limit: String(PAGE_SIZE) });
  if (cursor) params.set("cursor", cursor);
  const res = await apiRequest(`/nodes/${encodeURIComponent(id)}/children?${params}`, { signal });
  return expectShape(res, isChildrenPage, "GET /nodes/{id}/children");
}

// A plain link: the backend answers with Content-Disposition: attachment,
// so the browser downloads it without leaving the page.
export function contentUrl(id: string): string {
  return `/api/v1/nodes/${encodeURIComponent(id)}/content`;
}

export function archiveUrl(id: string): string {
  return `/api/v1/nodes/${encodeURIComponent(id)}/archive`;
}

// Starts a download without leaving the page: the backend answers with
// Content-Disposition: attachment, so following the link saves the file.
export function download(url: string): void {
  const a = document.createElement("a");
  a.href = url;
  a.rel = "noopener";
  document.body.append(a);
  a.click();
  a.remove();
}

export async function createFolder(parentId: string, name: string): Promise<FileNode> {
  const res = await apiRequest(`/nodes/${encodeURIComponent(parentId)}/children`, { method: "POST", body: { name } });
  return expectShape(res, isFileNode, "POST /nodes/{id}/children");
}

export async function renameNode(id: string, name: string): Promise<FileNode> {
  const res = await apiRequest(`/nodes/${encodeURIComponent(id)}`, { method: "PATCH", body: { name } });
  return expectShape(res, isFileNode, "PATCH /nodes/{id}");
}

export async function restoreNode(id: string): Promise<FileNode> {
  const res = await apiRequest(`/trash/${encodeURIComponent(id)}/restore`, { method: "POST" });
  return expectShape(res, isFileNode, "POST /trash/{id}/restore");
}

export type BatchOp = { op: "move" | "copy"; id: string; parentId: string } | { op: "delete"; id: string };

// One batch operation's outcome: an error code, or none for success; a
// big copy goes on in the background as job.
export type BatchResult = { id: string; error?: string; job?: number };

// As the backend sends it: a node on success, {code, message} on failure,
// a job id for a background copy.
type WireBatchResult = { id: string; error?: { code: string }; job?: number };

function isWireBatchResult(v: unknown): v is WireBatchResult {
  return (
    isRecord(v) &&
    isString(v.id) &&
    (v.error === undefined || (isRecord(v.error) && isString(v.error.code))) &&
    (v.job === undefined || isNumber(v.job))
  );
}

function isBatchResponse(v: unknown): v is { results: WireBatchResult[] } {
  return isRecord(v) && isArrayOf(v.results, isWireBatchResult);
}

// Runs ops on the backend, each on its own: some may fail while the rest
// go through (a name conflict, say), and the results say which.
export async function runBatch(ops: BatchOp[]): Promise<BatchResult[]> {
  const res = expectShape(await apiRequest("/nodes/batch", { method: "POST", body: { ops } }), isBatchResponse, "POST /nodes/batch");
  return res.results.map((r) => ({ id: r.id, error: r.error?.code, job: r.job }));
}

// A background copy's state; error is a code when it failed.
export type JobStatus = { state: "running" | "done" | "failed"; error?: string };

function isJobStatus(v: unknown): v is JobStatus {
  return isRecord(v) && (v.state === "running" || v.state === "done" || v.state === "failed") && (v.error === undefined || isString(v.error));
}

export async function fetchJob(id: number, signal?: AbortSignal): Promise<JobStatus> {
  return expectShape(await apiRequest(`/jobs/${id}`, { signal }), isJobStatus, "GET /jobs/{id}");
}

// A node in the trash, with where it was deleted from ("" for the root).
export type TrashItem = FileNode & { trashedAt: string; originalPath: string };

export type TrashPage = { items: TrashItem[]; nextCursor: string | null };

function isTrashItem(v: unknown): v is TrashItem {
  return isRecord(v) && isString(v.trashedAt) && isString(v.originalPath) && isFileNode(v);
}

function isTrashPage(v: unknown): v is TrashPage {
  return isRecord(v) && isArrayOf(v.items, isTrashItem) && (v.nextCursor === null || isString(v.nextCursor));
}

export const trashQueryKey = ["trash"] as const;

export async function fetchTrash(cursor: string | null, signal?: AbortSignal): Promise<TrashPage> {
  const params = new URLSearchParams({ limit: String(PAGE_SIZE) });
  if (cursor) params.set("cursor", cursor);
  return expectShape(await apiRequest(`/trash?${params}`, { signal }), isTrashPage, "GET /trash");
}

export async function purgeNode(id: string): Promise<void> {
  await apiRequest(`/trash/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export async function emptyTrash(): Promise<void> {
  await apiRequest("/trash", { method: "DELETE" });
}

// A node whose name matched a search, with the folder it is in: its path
// from the root ("Docs/2026", "" for the root) and its id.
export type SearchResult = FileNode & { path: string; parentId: string };

export type SearchPage = { items: SearchResult[]; nextCursor: string | null };

// The backend refuses shorter queries (query_too_short): fewer characters
// can't use its trigram index.
export const MIN_SEARCH_QUERY = 3;

function isSearchResult(v: unknown): v is SearchResult {
  return isRecord(v) && isString(v.path) && isString(v.parentId) && isFileNode(v);
}

function isSearchPage(v: unknown): v is SearchPage {
  return isRecord(v) && isArrayOf(v.items, isSearchResult) && (v.nextCursor === null || isString(v.nextCursor));
}

export function searchQueryKey(q: string) {
  return ["search", q] as const;
}

export async function fetchSearch(q: string, cursor: string | null, signal?: AbortSignal): Promise<SearchPage> {
  const params = new URLSearchParams({ q });
  if (cursor) params.set("cursor", cursor);
  return expectShape(await apiRequest(`/search?${params}`, { signal }), isSearchPage, "GET /search");
}

// Who wrote some content; null where that's unknown (older files, deleted
// accounts).
export type Author = { id: string; displayName: string } | null;

function isAuthor(v: unknown): v is Author {
  return v === null || (isRecord(v) && isString(v.id) && isString(v.displayName));
}

// An earlier content of a file; the current one is the file itself.
export type Version = { id: string; size: number; createdAt: string; author: Author };

function isVersion(v: unknown): v is Version {
  return isRecord(v) && isString(v.id) && isNumber(v.size) && isString(v.createdAt) && isAuthor(v.author);
}

export type Versions = { items: Version[]; currentAuthor: Author };

function isVersions(v: unknown): v is Versions {
  return isRecord(v) && isArrayOf(v.items, isVersion) && isAuthor(v.currentAuthor);
}

export function versionsQueryKey(id: string) {
  return ["versions", id] as const;
}

export async function fetchVersions(id: string, signal?: AbortSignal): Promise<Versions> {
  const res = await apiRequest(`/nodes/${encodeURIComponent(id)}/versions`, { signal });
  return expectShape(res, isVersions, "GET /nodes/{id}/versions");
}

export async function restoreVersion(id: string): Promise<FileNode> {
  const res = await apiRequest(`/versions/${encodeURIComponent(id)}/restore`, { method: "POST" });
  return expectShape(res, isFileNode, "POST /versions/{id}/restore");
}

export function versionContentUrl(id: string): string {
  return `/api/v1/versions/${encodeURIComponent(id)}/content`;
}

export type GranteeType = "user" | "group";

export type Grantee = { type: GranteeType; id: string; name: string };

export type ShareGrant = { id: string; grantee: Grantee; permissions: Permission[]; createdAt: string };

// A node someone shared with the signed-in user.
export type SharedItem = FileNode & { owner: Grantee; sharedAt: string };

function isGrantee(v: unknown): v is Grantee {
  return isRecord(v) && (v.type === "user" || v.type === "group") && isString(v.id) && isString(v.name);
}

function isShareGrant(v: unknown): v is ShareGrant {
  return isRecord(v) && isString(v.id) && isGrantee(v.grantee) && isArrayOf(v.permissions, isPermission) && isString(v.createdAt);
}

function isSharedItem(v: unknown): v is SharedItem {
  return isRecord(v) && isGrantee(v.owner) && isString(v.sharedAt) && isFileNode(v);
}

function itemsOf<T>(guard: (v: unknown) => v is T) {
  return (v: unknown): v is { items: T[] } => isRecord(v) && isArrayOf(v.items, guard);
}

export function sharesQueryKey(id: string) {
  return ["shares", id] as const;
}

export const sharedWithMeQueryKey = ["sharedWithMe"] as const;

export async function fetchShares(nodeId: string, signal?: AbortSignal): Promise<ShareGrant[]> {
  const res = await apiRequest(`/nodes/${encodeURIComponent(nodeId)}/shares`, { signal });
  return expectShape(res, itemsOf(isShareGrant), "GET /nodes/{id}/shares").items;
}

export async function grantShare(nodeId: string, grantee: Grantee, permissions: Permission[]): Promise<ShareGrant> {
  const res = await apiRequest(`/nodes/${encodeURIComponent(nodeId)}/shares`, {
    method: "POST",
    body: { granteeType: grantee.type, granteeId: grantee.id, permissions },
  });
  return expectShape(res, isShareGrant, "POST /nodes/{id}/shares");
}

export async function updateShare(id: string, permissions: Permission[]): Promise<ShareGrant> {
  const res = await apiRequest(`/shares/${encodeURIComponent(id)}`, { method: "PATCH", body: { permissions } });
  return expectShape(res, isShareGrant, "PATCH /shares/{id}");
}

export async function revokeShare(id: string): Promise<void> {
  await apiRequest(`/shares/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export async function fetchSharedWithMe(signal?: AbortSignal): Promise<SharedItem[]> {
  return expectShape(await apiRequest("/shared-with-me", { signal }), itemsOf(isSharedItem), "GET /shared-with-me").items;
}

export async function searchDirectory(q: string, signal?: AbortSignal): Promise<Grantee[]> {
  const res = await apiRequest(`/directory?q=${encodeURIComponent(q)}`, { signal });
  return expectShape(res, itemsOf(isGrantee), "GET /directory").items;
}

export type LinkMode = "view" | "download" | "upload";

export type PublicLink = {
  id: string;
  // null when the server can't unseal it any more (SECRET_KEY changed).
  token: string | null;
  mode: LinkMode;
  hasPassword: boolean;
  expiresAt: string | null;
  createdAt: string;
};

function isLinkMode(v: unknown): v is LinkMode {
  return v === "view" || v === "download" || v === "upload";
}

function isPublicLink(v: unknown): v is PublicLink {
  return (
    isRecord(v) &&
    isString(v.id) &&
    (v.token === null || isString(v.token)) &&
    isLinkMode(v.mode) &&
    typeof v.hasPassword === "boolean" &&
    (v.expiresAt === null || isString(v.expiresAt)) &&
    isString(v.createdAt)
  );
}

export function linksQueryKey(id: string) {
  return ["links", id] as const;
}

export async function fetchLinks(nodeId: string, signal?: AbortSignal): Promise<PublicLink[]> {
  const res = await apiRequest(`/nodes/${encodeURIComponent(nodeId)}/links`, { signal });
  return expectShape(res, itemsOf(isPublicLink), "GET /nodes/{id}/links").items;
}

export async function createLink(nodeId: string, mode: LinkMode, password: string, expiresAt: string | null): Promise<PublicLink> {
  const res = await apiRequest(`/nodes/${encodeURIComponent(nodeId)}/links`, {
    method: "POST",
    body: { mode, ...(password ? { password } : {}), ...(expiresAt ? { expiresAt } : {}) },
  });
  return expectShape(res, isPublicLink, "POST /nodes/{id}/links");
}

export async function deleteLink(id: string): Promise<void> {
  await apiRequest(`/links/${encodeURIComponent(id)}`, { method: "DELETE" });
}

// The address to hand out for a link.
export function linkUrl(token: string): string {
  return `${window.location.origin}/s/${token}`;
}
