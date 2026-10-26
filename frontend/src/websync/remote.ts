import { Upload, type UploadOptions as TusOptions } from "tus-js-client";
import { ApiError, apiRequest, toApiError } from "../api";
import { contentUrl, createFolder, type FileNode, type NodeKind, type Permission } from "../files";
import { expectShape, isArrayOf, isNumber, isRecord, isString, UnexpectedResponseError } from "../guards";
import { CHUNK_SIZE, RETRY_DELAYS, UPLOAD_ENDPOINT, uploadError } from "../uploads";

// The server's sync API (backend/internal/sync) and the conditional writes
// a sync client makes (docs/desktop-client.md, «Условные записи»).

export type SyncRoot = {
  id: string;
  kind: "own" | "shared";
  // "" for the own root; a share's as in WebDAV's «Доступно мне».
  name: string;
  permissions: Permission[];
};

// A node as /sync/tree streams it.
export type TreeNode = {
  id: string;
  parentId: string | null;
  name: string;
  kind: NodeKind;
  size: number;
  etag: string;
  sha256?: string;
  permissions: Permission[];
};

function isPermission(v: unknown): v is Permission {
  return v === "read" || v === "write" || v === "reshare";
}

function isSyncRoot(v: unknown): v is SyncRoot {
  return isRecord(v) && isString(v.id) && (v.kind === "own" || v.kind === "shared") && isString(v.name) && isArrayOf(v.permissions, isPermission);
}

function isTreeNode(v: unknown): v is TreeNode {
  return (
    isRecord(v) &&
    isString(v.id) &&
    (v.parentId === null || isString(v.parentId)) &&
    isString(v.name) &&
    (v.kind === "dir" || v.kind === "file") &&
    isNumber(v.size) &&
    isString(v.etag) &&
    (v.sha256 === undefined || isString(v.sha256)) &&
    isArrayOf(v.permissions, isPermission)
  );
}

export async function fetchRoots(signal?: AbortSignal): Promise<SyncRoot[]> {
  const res = expectShape(await apiRequest("/sync/roots", { signal }), (v): v is { roots: SyncRoot[] } => isRecord(v) && isArrayOf(v.roots, isSyncRoot), "GET /sync/roots");
  return res.roots;
}

// fetchTree reads a whole subtree (root: a node id or "root"), the root
// first and every folder before its contents. The server cuts the response
// off if it fails midway, so a stream that ends without error is the whole
// tree; anything else throws — a pass never plans from half a tree, which
// would look like deletions.
export async function fetchTree(root: string, signal?: AbortSignal): Promise<TreeNode[]> {
  const url = `/api/v1/sync/tree?root=${encodeURIComponent(root)}`;
  let res: Response;
  try {
    res = await fetch(url, { signal, credentials: "same-origin", headers: { Accept: "application/x-ndjson" } });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    throw new ApiError(0, "network", `GET ${url}: ${String(err)}`);
  }
  if (!res.ok) throw await toApiError(res, `GET ${url}`);
  const nodes: TreeNode[] = [];
  let sawCursor = false;
  const line = (text: string) => {
    if (text.trim() === "") return;
    const v: unknown = JSON.parse(text);
    if (!sawCursor) {
      if (!isRecord(v) || !isString(v.cursor)) throw new UnexpectedResponseError("GET /sync/tree: no cursor first");
      sawCursor = true;
      return;
    }
    nodes.push(expectShape(v, isTreeNode, "GET /sync/tree"));
  };
  if (!res.body) {
    for (const l of (await res.text()).split("\n")) line(l);
  } else {
    const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
    let rest = "";
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      rest += value;
      const lines = rest.split("\n");
      rest = lines.pop() ?? "";
      for (const l of lines) line(l);
    }
    line(rest);
  }
  if (nodes.length === 0) throw new UnexpectedResponseError("GET /sync/tree: no root");
  return nodes;
}

// download opens a file's content.
export async function download(nodeId: string, signal?: AbortSignal): Promise<ReadableStream<Uint8Array<ArrayBuffer>>> {
  const url = contentUrl(nodeId);
  let res: Response;
  try {
    res = await fetch(url, { signal, credentials: "same-origin" });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    throw new ApiError(0, "network", `GET ${url}: ${String(err)}`);
  }
  if (!res.ok) throw await toApiError(res, `GET ${url}`);
  if (!res.body) throw new UnexpectedResponseError(`GET ${url}: no body`);
  return res.body;
}

function quoted(etag: string): string {
  return `"${etag}"`;
}

// trash moves a node to the trash; with ifMatch only if it still has that
// ETag (else 412 precondition_failed).
export async function trash(nodeId: string, ifMatch?: string): Promise<void> {
  await apiRequest(`/nodes/${encodeURIComponent(nodeId)}`, { method: "DELETE", headers: ifMatch ? { "If-Match": quoted(ifMatch) } : undefined });
}

function isNode(v: unknown): v is { id: string; etag: string } {
  return isRecord(v) && isString(v.id) && isString(v.etag);
}

// move puts a file into parentId under name, if it still has ifMatch.
export async function move(nodeId: string, parentId: string, name: string, ifMatch: string): Promise<{ id: string; etag: string }> {
  const res = await apiRequest(`/nodes/${encodeURIComponent(nodeId)}`, { method: "PATCH", body: { parentId, name }, headers: { "If-Match": quoted(ifMatch) } });
  return expectShape(res, isNode, "PATCH /nodes/{id}");
}

export async function mkdir(parentId: string, name: string): Promise<FileNode> {
  return createFolder(parentId, name);
}

// isEmpty: the folder has nothing in it now.
export async function isEmpty(nodeId: string, signal?: AbortSignal): Promise<boolean> {
  const res = await apiRequest(`/nodes/${encodeURIComponent(nodeId)}/children?limit=1`, { signal });
  return expectShape(res, (v): v is { items: unknown[] } => isRecord(v) && Array.isArray(v.items), "GET /nodes/{id}/children").items.length === 0;
}

export type UploadTarget =
  // A new file in parentId; a taken name fails (409 name_conflict).
  | { parentId: string; name: string }
  // A new version of nodeId, if it still has the ETag ifMatch.
  | { nodeId: string; ifMatch: string };

export type Uploaded = { nodeId: string; etag: string };

export type UploadOptions = {
  sha256?: string;
  signal?: AbortSignal;
  onProgress?: (sent: number) => void;
  // Tests talk to a fake backend through fetch, with their own reader of
  // the file.
  tus?: Pick<TusOptions, "httpStack" | "fileReader">;
};

// upload sends a file over tus with the sync metadata: the file's mtime,
// its SHA-256 for the server to check, and the condition of an update.
// The answer's Upload-Node-Id and Upload-Node-ETag are the next base.
export function upload(file: File, target: UploadTarget, options: UploadOptions = {}): Promise<Uploaded> {
  const metadata: Record<string, string> = { mtime: new Date(file.lastModified).toISOString() };
  if ("nodeId" in target) {
    metadata.nodeId = target.nodeId;
    metadata.ifMatch = target.ifMatch;
  } else {
    metadata.parentId = target.parentId;
    metadata.filename = target.name;
    metadata.onConflict = "fail";
  }
  if (options.sha256) metadata.sha256 = options.sha256;
  return new Promise<Uploaded>((resolve, reject) => {
    const up = new Upload(file, {
      endpoint: UPLOAD_ENDPOINT,
      chunkSize: CHUNK_SIZE,
      retryDelays: RETRY_DELAYS,
      metadata,
      // A pass uploads what it planned with the metadata it planned: an
      // upload left from an earlier one (another base) mustn't be resumed.
      storeFingerprintForResuming: false,
      ...options.tus,
      // Only the network and the server's own trouble get better by
      // waiting; 412, 409, 423 and a full quota are the next pass's.
      onShouldRetry: (err) => {
        const status = err.originalResponse?.getStatus() ?? 0;
        return status === 0 || status >= 500;
      },
      onProgress: (sent) => options.onProgress?.(sent),
      onSuccess: ({ lastResponse }) => {
        const nodeId = lastResponse.getHeader("Upload-Node-Id");
        const etag = lastResponse.getHeader("Upload-Node-ETag");
        if (nodeId && etag) resolve({ nodeId, etag });
        else reject(new UnexpectedResponseError("tus: no Upload-Node-Id/ETag"));
      },
      onError: (err) => reject(uploadError(err)),
    });
    const abort = () => {
      // abort(true) also deletes the upload on the server, ending its
      // quota reservation.
      void up.abort(true).catch(() => undefined);
      reject(new DOMException("upload aborted", "AbortError"));
    };
    if (options.signal?.aborted) return abort();
    options.signal?.addEventListener("abort", abort, { once: true });
    up.start();
  });
}
