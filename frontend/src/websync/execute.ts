import { ApiError } from "../api";
import {
  ItemError,
  isPermissionError,
  moveFile,
  readFile,
  removeDirIfEmpty,
  removeFile,
  writeFile,
  dirAt,
  type DirHandle,
} from "./localfs";
import { baseName } from "./names";
import { opPhase, type BaseItem, type Op, type Problem, type ProblemKind, type Stamp } from "./plan";
import type { UploadOptions, UploadTarget, Uploaded } from "./remote";

// Runs a plan: folders first, top-down; then moves; then transfers, a few
// at a time; then — only if the user agreed — deletes, bottom-up. Every
// op is idempotent and records its outcome in base as soon as it is made,
// so a pass cut off (cancel, a closed tab) leaves base true to what was
// done, and the next pass carries on from there.

export type Api = {
  download(nodeId: string, signal?: AbortSignal): Promise<ReadableStream<Uint8Array<ArrayBuffer>>>;
  upload(file: File, target: UploadTarget, options?: UploadOptions): Promise<Uploaded>;
  trash(nodeId: string, ifMatch?: string): Promise<void>;
  move(nodeId: string, parentId: string, name: string, ifMatch: string): Promise<{ id: string; etag: string }>;
  mkdir(parentId: string, name: string): Promise<{ id: string; etag: string }>;
  isEmpty(nodeId: string): Promise<boolean>;
};

export type Progress = {
  done: number;
  total: number;
  bytes: number;
  totalBytes: number;
  // The path being worked on.
  current?: string;
};

export type ExecOptions = {
  root: DirHandle;
  api: Api;
  // Node ids of the folders by key, from the tree: where uploads and new
  // folders go. Folders made by the pass are added.
  ids: Map<string, string>;
  // The user agreed to the deletes that ask for it.
  deletes: boolean;
  saveBase: (put: BaseItem[], remove: string[]) => Promise<void>;
  hash: (file: File, signal?: AbortSignal) => Promise<string>;
  signal?: AbortSignal;
  onProgress?: (p: Progress) => void;
  parallel?: number;
};

export type Outcome = {
  uploaded: number;
  downloaded: number;
  conflicts: number;
  deletedRemote: number;
  deletedLocal: number;
  // Deletes left for later: the user didn't agree to them.
  deferred: number;
  problems: Problem[];
};

// The whole pass stops: the browser took the folder's permission away.
export class PermissionLost extends Error {
  constructor(cause: unknown) {
    super(`permission lost: ${String(cause)}`);
    this.name = "PermissionLost";
  }
}

const API_PROBLEMS: Record<string, ProblemKind> = {
  precondition_failed: "precondition",
  quota_exceeded: "quota",
  locked: "locked",
  name_conflict: "nameConflict",
  forbidden: "forbidden",
  not_found: "changedMeanwhile",
  checksum_mismatch: "changedMeanwhile",
};

function problemOf(err: unknown, path: string): Problem {
  if (err instanceof ItemError) return { kind: err.kind, path, code: err.code };
  if (err instanceof ApiError) return { kind: API_PROBLEMS[err.code] ?? "failed", path, code: err.code };
  return { kind: "failed", path };
}

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}

// Whether an op moves bytes, and how many.
function bytesOf(op: Op): number {
  switch (op.type) {
    case "download":
    case "upload":
      return op.size;
    case "conflict":
      return op.size + (op.upload ? op.expect.size : 0);
    default:
      return 0;
  }
}

export async function execute(ops: Op[], o: ExecOptions): Promise<Outcome> {
  const out: Outcome = { uploaded: 0, downloaded: 0, conflicts: 0, deletedRemote: 0, deletedLocal: 0, deferred: 0, problems: [] };
  const runnable = ops.filter((op) => o.deletes || !(op.type === "trashRemote" || (op.type === "removeLocal" && op.confirm)));
  out.deferred = ops.length - runnable.length;
  const progress: Progress = { done: 0, total: runnable.length, bytes: 0, totalBytes: runnable.reduce((n, op) => n + bytesOf(op), 0) };
  const report = (current?: string) => o.onProgress?.({ ...progress, current });
  // Nodes an op failed for: what was to follow them (removing the old
  // copy of a moved file) waits for the next pass.
  const failed = new Set<string>();

  let put = new Map<string, BaseItem>();
  let remove = new Set<string>();
  const setBase = (b: BaseItem) => {
    remove.delete(b.key);
    put.set(b.key, b);
  };
  const dropBase = (key: string) => {
    put.delete(key);
    remove.add(key);
  };
  const flush = async () => {
    if (put.size === 0 && remove.size === 0) return;
    const p = [...put.values()];
    const r = [...remove];
    put = new Map();
    remove = new Set();
    await o.saveBase(p, r);
  };

  const parentId = (key: string): string => {
    const id = o.ids.get(key);
    if (!id) throw new ItemError("changedMeanwhile", `no folder for ${key || "/"}`);
    return id;
  };

  const download = async (key: string, path: string, nodeId: string, etag: string, size: number, sha256: string | undefined, expect: Stamp | undefined) => {
    let last = 0;
    const body = await o.api.download(nodeId, o.signal);
    const w = await writeFile(o.root, path, body, { size, sha256, expect }, o.signal, (n) => {
      last += n;
      progress.bytes += n;
      report(path);
    });
    // A download that wrote less than planned (it can't, but) keeps the
    // byte count honest.
    progress.bytes += size - last;
    setBase({ key, path, nodeId, kind: "file", etag, sha256: w.sha256, size: w.size, lastModified: w.lastModified });
  };

  const uploadFile = async (key: string, path: string, target: UploadTarget, file: File, sha256: string | undefined) => {
    const sum = sha256 ?? (await o.hash(file, o.signal));
    let last = 0;
    const up = await o.api.upload(file, target, {
      sha256: sum,
      signal: o.signal,
      onProgress: (sent) => {
        progress.bytes += sent - last;
        last = sent;
        report(path);
      },
    });
    progress.bytes += file.size - last;
    o.ids.set(key, up.nodeId);
    setBase({ key, path, nodeId: up.nodeId, kind: "file", etag: up.etag, sha256: sum, size: file.size, lastModified: file.lastModified });
  };

  const run = async (op: Op): Promise<void> => {
    switch (op.type) {
      case "setBase":
        setBase(op.base);
        return;
      case "forget":
        dropBase(op.key);
        return;
      case "mkdirLocal":
        await dirAt(o.root, op.path, true).catch((err: unknown) => {
          if (err instanceof TypeError) throw new ItemError("nameNotAllowed", `${op.path}: ${String(err)}`);
          throw err;
        });
        if (!op.container) setBase({ key: op.key, path: op.path, nodeId: op.nodeId, kind: "dir", etag: "", size: 0, lastModified: 0 });
        return;
      case "mkdirRemote": {
        const n = await o.api.mkdir(parentId(op.parentKey), op.name);
        o.ids.set(op.key, n.id);
        setBase({ key: op.key, path: op.path, nodeId: n.id, kind: "dir", etag: n.etag, size: 0, lastModified: 0 });
        return;
      }
      case "moveLocal": {
        const st = await moveFile(o.root, op.fromPath, op.path, op.expect);
        dropBase(op.fromKey);
        setBase({ key: op.key, path: op.path, nodeId: op.nodeId, kind: "file", etag: op.etag, sha256: op.sha256, ...st });
        return;
      }
      case "moveRemote": {
        const n = await o.api.move(op.nodeId, parentId(op.parentKey), op.name, op.ifMatch);
        dropBase(op.fromKey);
        setBase({ key: op.key, path: op.path, nodeId: op.nodeId, kind: "file", etag: n.etag, sha256: op.sha256, ...op.stamp });
        return;
      }
      case "download":
        await download(op.key, op.path, op.nodeId, op.etag, op.size, op.sha256, op.expect);
        out.downloaded++;
        return;
      case "upload": {
        const file = await readFile(o.root, op.path, op.expect);
        const target: UploadTarget = op.nodeId && op.ifMatch ? { nodeId: op.nodeId, ifMatch: op.ifMatch } : { parentId: parentId(op.parentKey), name: op.name };
        await uploadFile(op.key, op.path, target, file, op.sha256);
        out.uploaded++;
        return;
      }
      case "conflict": {
        // The edit steps aside first: from here on base has nothing at the
        // name, so a failure below can't make the next pass take the
        // missing file for a delete.
        const st = await moveFile(o.root, op.path, op.copyPath, op.expect);
        dropBase(op.key);
        out.conflicts++;
        if (op.upload) {
          const file = await readFile(o.root, op.copyPath, st);
          await uploadFile(op.copyKey, op.copyPath, { parentId: parentId(op.parentKey), name: baseName(op.copyPath) }, file, undefined);
        }
        await download(op.key, op.path, op.nodeId, op.etag, op.size, op.sha256, undefined);
        return;
      }
      case "trashRemote":
        if (op.kind === "dir" && !(await o.api.isEmpty(op.nodeId))) {
          // Something new arrived in it, or a file in it wasn't deleted:
          // the folder stays, and comes back next pass.
          throw new ItemError("folderNotEmpty", op.path);
        }
        await o.api.trash(op.nodeId, op.kind === "file" ? op.ifMatch : undefined);
        dropBase(op.baseKey ?? op.key);
        out.deletedRemote++;
        return;
      case "removeLocal":
        if (!op.confirm && failed.has(op.nodeId)) return;
        if (op.kind === "file") {
          await removeFile(o.root, op.path, op.expect);
        } else if (!(await removeDirIfEmpty(o.root, op.path))) {
          if (op.confirm) throw new ItemError("folderNotEmpty", op.path);
          dropBase(op.key);
          return;
        }
        dropBase(op.key);
        if (op.confirm) out.deletedLocal++;
        return;
    }
  };

  const nodeOf = (op: Op): string | undefined => {
    if (op.type === "setBase") return op.base.nodeId;
    if (op.type === "forget" || op.type === "mkdirRemote") return undefined;
    return op.nodeId;
  };

  const attempt = async (op: Op) => {
    const path = op.type === "setBase" ? op.base.path : op.type === "forget" ? op.key : op.path;
    report(path);
    try {
      await run(op);
    } catch (err) {
      if (isAbort(err)) throw err;
      if (isPermissionError(err)) throw new PermissionLost(err);
      const n = nodeOf(op);
      if (n) failed.add(n);
      out.problems.push(problemOf(err, path));
    } finally {
      progress.done++;
      report();
    }
  };

  try {
    for (let phase = 0; phase <= 3; phase++) {
      const these = runnable.filter((op) => opPhase(op) === phase);
      if (phase === 2) {
        // Transfers overlap: a few at a time keeps the link busy without
        // one big file starving the rest.
        let next = 0;
        let stopped = false;
        const worker = async () => {
          try {
            while (next < these.length && !stopped) {
              o.signal?.throwIfAborted();
              await attempt(these[next++]);
              if (put.size + remove.size >= 200) await flush();
            }
          } catch (err) {
            // The others finish what they are on and stop too.
            stopped = true;
            throw err;
          }
        };
        await Promise.all(Array.from({ length: o.parallel ?? 3 }, worker));
      } else {
        for (const op of these) {
          o.signal?.throwIfAborted();
          await attempt(op);
        }
      }
      await flush();
    }
  } finally {
    // Whatever was done stays recorded, cancelled or not.
    await flush();
  }
  return out;
}
