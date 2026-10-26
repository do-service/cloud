import { linkRequest } from "./api";
import { isFileNode, type ChildrenPage, type FileNode, type NodeWithPath, type Sort } from "./files";
import { expectShape, isArrayOf, isBoolean, isRecord, isString } from "./guards";
import type { PreviewSize } from "./preview";

// The public side of a link (/s/{token}): what it is, its password, and
// browsing what it opens to. Paths here are the link's own, not /api/v1.

export type LinkInfo = {
  name: string;
  kind: "dir" | "file";
  mode: "view" | "download" | "upload";
  needsPassword: boolean;
  unlocked: boolean;
  expiresAt: string | null;
  rootId: string;
};

function isLinkInfo(v: unknown): v is LinkInfo {
  return (
    isRecord(v) &&
    isString(v.name) &&
    (v.kind === "dir" || v.kind === "file") &&
    (v.mode === "view" || v.mode === "download" || v.mode === "upload") &&
    isBoolean(v.needsPassword) &&
    isBoolean(v.unlocked) &&
    (v.expiresAt === null || isString(v.expiresAt)) &&
    isString(v.rootId)
  );
}

function isNodeWithPath(v: unknown): v is NodeWithPath {
  return isRecord(v) && isArrayOf(v.path, (p): p is { id: string; name: string } => isRecord(p) && isString(p.id) && isString(p.name)) && isFileNode(v);
}

function isChildrenPage(v: unknown): v is ChildrenPage {
  return isRecord(v) && isArrayOf(v.items, isFileNode) && (v.nextCursor === null || isString(v.nextCursor));
}

export function linkInfoQueryKey(token: string) {
  return ["link", token, "info"] as const;
}

export async function fetchLinkInfo(token: string, signal?: AbortSignal): Promise<LinkInfo> {
  return expectShape(await linkRequest(token, "/info", { signal }), isLinkInfo, "GET /s/{token}/api/info");
}

export async function unlockLink(token: string, password: string): Promise<void> {
  await linkRequest(token, "/unlock", { method: "POST", body: { password } });
}

export async function fetchLinkNode(token: string, id: string, signal?: AbortSignal): Promise<NodeWithPath> {
  return expectShape(await linkRequest(token, `/nodes/${encodeURIComponent(id)}`, { signal }), isNodeWithPath, "GET /s/{token}/api/nodes/{id}");
}

export async function fetchLinkChildren(token: string, id: string, sort: Sort, cursor: string | null, signal?: AbortSignal): Promise<ChildrenPage> {
  const params = new URLSearchParams({ sort: sort.key, order: sort.order, limit: "200" });
  if (cursor) params.set("cursor", cursor);
  const res = await linkRequest(token, `/nodes/${encodeURIComponent(id)}/children?${params}`, { signal });
  return expectShape(res, isChildrenPage, "GET /s/{token}/api/nodes/{id}/children");
}

export function linkContentUrl(token: string, id: string): string {
  return `/s/${encodeURIComponent(token)}/api/nodes/${encodeURIComponent(id)}/content`;
}

// A link's preview of a file, as previewUrl for the signed-in.
export function linkPreviewUrl(token: string, id: string, size: PreviewSize, etag: string): string {
  return `/s/${encodeURIComponent(token)}/api/nodes/${encodeURIComponent(id)}/preview?${new URLSearchParams({ size, v: etag })}`;
}

// A file shown rather than downloaded, for the viewer: a view link shows
// anyway, a download link when asked.
export function linkInlineUrl(token: string, id: string): string {
  return `${linkContentUrl(token, id)}?inline=1`;
}

export function linkArchiveUrl(token: string, id: string): string {
  return `/s/${encodeURIComponent(token)}/api/nodes/${encodeURIComponent(id)}/archive`;
}

export function linkUploadEndpoint(token: string): string {
  return `/s/${encodeURIComponent(token)}/api/uploads/`;
}

// Types a view link shows in the browser; the server serves only these
// inline and refuses the rest.
export function viewable(n: FileNode): boolean {
  const mime = (n.mime ?? "").split(";")[0].trim().toLowerCase();
  return (
    ["image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/bmp", "application/pdf", "text/plain"].includes(mime) ||
    mime.startsWith("video/") ||
    mime.startsWith("audio/")
  );
}
