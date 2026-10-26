import { execute, type Api, type Outcome, type Progress } from "./execute";
import { fileAt, isNotFound, isPermissionError, scan, type DirHandle, type PermissionMode } from "./localfs";
import { makePlan, needsHash, inArea, sameStamp, type BaseItem, type Op, type Plan, type Problem } from "./plan";
import { download, fetchRoots, fetchTree, isEmpty, mkdir, move, trash, upload, type SyncRoot, type TreeNode } from "./remote";
import { buildRemote, type RemoteTree } from "./snapshot";
import { wantsTree, type Rule } from "./selection";
import { hashFile } from "./sha256";
import type { SyncStore } from "./store";

// One pass of the browser's folder sync, on the button: scan both sides,
// plan, and — after the user has seen what would be deleted — run.

// Above either, the deletes of a pass get a warning of their own and an
// explicit "I've checked" (as the desktop client's pause, «Защита»).
export const MASS_DELETE_FILES = 500;
export const MASS_DELETE_SHARE = 0.3;
// A share of a handful isn't "mass": deleting 1 file of 2 is ordinary.
export const MASS_DELETE_MIN = 10;
// With at least this many files synced before, a folder with none of them
// is taken for the wrong one (another picked, a disk not plugged in), and
// the pass refuses rather than offer to delete them all in the cloud.
export const EMPTY_FOLDER_MIN = 3;

export type Stage =
  | { stage: "remote" }
  | { stage: "scan"; items: number }
  | { stage: "hash"; done: number; total: number }
  | { stage: "plan" };

export type Deletion = { path: string; kind: "file" | "dir" };

export type Summary = {
  upload: number;
  uploadBytes: number;
  download: number;
  downloadBytes: number;
  conflicts: number;
  // What the pass would delete, if the user agrees.
  deleteRemote: Deletion[];
  deleteLocal: Deletion[];
  // A large share of what was synced would go.
  mass: boolean;
};

export type Prepared = {
  plan: Plan;
  ids: Map<string, string>;
  summary: Summary;
  problems: Problem[];
};

export type PassContext = {
  store: SyncStore;
  root: DirHandle;
  folderId: string;
  rules: Rule[];
  // The local name of «Доступно мне».
  sharedName: string;
  conflictTag: (n: number) => string;
  windows: boolean;
  api: Api & {
    roots: (signal?: AbortSignal) => Promise<SyncRoot[]>;
    tree: (root: string, signal?: AbortSignal) => Promise<TreeNode[]>;
  };
  signal?: AbortSignal;
};

// The folder looks empty while base says much was synced into it.
export class FolderLooksEmpty extends Error {
  constructor(readonly synced: number) {
    super(`the folder is empty, but ${synced} files were synced into it`);
    this.name = "FolderLooksEmpty";
  }
}

export class FolderGone extends Error {
  constructor() {
    super("the folder is gone");
    this.name = "FolderGone";
  }
}

export function summarize(ops: Op[], base: BaseItem[], area: (key: string) => boolean): Summary {
  const s: Summary = { upload: 0, uploadBytes: 0, download: 0, downloadBytes: 0, conflicts: 0, deleteRemote: [], deleteLocal: [], mass: false };
  for (const op of ops) {
    switch (op.type) {
      case "upload":
        s.upload++;
        s.uploadBytes += op.size;
        break;
      case "download":
        s.download++;
        s.downloadBytes += op.size;
        break;
      case "conflict":
        s.conflicts++;
        s.downloadBytes += op.size;
        if (op.upload) s.uploadBytes += op.expect.size;
        break;
      case "trashRemote":
        s.deleteRemote.push({ path: op.path, kind: op.kind });
        break;
      case "removeLocal":
        if (op.confirm) s.deleteLocal.push({ path: op.path, kind: op.kind });
        break;
    }
  }
  const files = s.deleteRemote.filter((d) => d.kind === "file").length + s.deleteLocal.filter((d) => d.kind === "file").length;
  const synced = base.filter((b) => b.kind === "file" && area(b.key)).length;
  s.mass = files > MASS_DELETE_FILES || (files >= MASS_DELETE_MIN && files > MASS_DELETE_SHARE * synced);
  return s;
}

// prepare scans and plans a pass, deleting nothing yet.
export async function prepare(ctx: PassContext, onStage?: (s: Stage) => void): Promise<Prepared> {
  const { signal } = ctx;
  onStage?.({ stage: "remote" });
  const roots = await ctx.api.roots(signal);
  const own = roots.find((r) => r.kind === "own");
  const trees: Promise<RemoteTree | undefined>[] = roots.map(async (root) => {
    if (!wantsTree(ctx.rules, root.id, root.kind === "shared")) return undefined;
    return { root, nodes: await ctx.api.tree(root.id, signal) };
  });
  const fetched = await Promise.all(trees);
  const base = await ctx.store.base(ctx.folderId);
  const view = buildRemote({
    own: fetched.find((t) => t?.root.id === own?.id),
    shares: fetched.filter((t): t is RemoteTree => t !== undefined && t.root.kind === "shared"),
    rules: ctx.rules,
    sharedName: ctx.sharedName,
    windows: ctx.windows,
    base,
  });
  const area = (key: string) => inArea(view.area, key);

  onStage?.({ stage: "scan", items: 0 });
  let local;
  try {
    local = await scan(ctx.root, { include: area, descend: (k) => view.descend.has(k) }, signal, (items) => onStage?.({ stage: "scan", items }));
  } catch (err) {
    if (isNotFound(err)) throw new FolderGone();
    throw err;
  }
  const synced = base.filter((b) => b.kind === "file" && area(b.key)).length;
  if (synced >= EMPTY_FOLDER_MIN && local.items.length === 0) throw new FolderLooksEmpty(synced);

  const toHash = needsHash(local.items, base, view.area);
  for (let i = 0; i < toHash.length; i++) {
    onStage?.({ stage: "hash", done: i, total: toHash.length });
    const l = toHash[i];
    try {
      const f = await (await fileAt(ctx.root, l.path)).getFile();
      // Changed again while the pass looked: hashed as it is now; the stamp
      // follows, so the plan's writes expect what was hashed.
      if (!sameStamp(f, l)) {
        l.size = f.size;
        l.lastModified = f.lastModified;
      }
      l.sha256 = await hashFile(f, signal);
    } catch (err) {
      if (isPermissionError(err)) throw err;
      if (signal?.aborted) throw err;
      // Unreadable now (gone, locked by another program): planned as an
      // edit of unknown content, which at worst re-uploads it.
    }
  }
  onStage?.({ stage: "plan" });
  const plan = makePlan({ remote: view.remote, local: local.items, base, area: view.area, conflictTag: ctx.conflictTag });
  const ids = new Map(view.remote.map((r) => [r.key, r.id]));
  return { plan, ids, summary: summarize(plan.ops, base, area), problems: [...view.problems, ...local.problems, ...plan.problems] };
}

// run carries out a prepared pass; deletes only if the user agreed.
export async function run(ctx: PassContext, prepared: Prepared, deletes: boolean, onProgress?: (p: Progress) => void): Promise<Outcome> {
  const outcome = await execute(prepared.plan.ops, {
    root: ctx.root,
    api: ctx.api,
    ids: prepared.ids,
    deletes,
    saveBase: (put, remove) => ctx.store.writeBase(ctx.folderId, put, remove),
    hash: (file, signal) => hashFile(file, signal),
    signal: ctx.signal,
    onProgress,
  });
  outcome.problems.unshift(...prepared.problems);
  return outcome;
}

// The server's API, as a pass uses it.
export const serverApi: PassContext["api"] = { roots: fetchRoots, tree: fetchTree, download, upload, trash, move, mkdir, isEmpty };

const LOCK = "cloud-websync";
let busyHere = false;

// acquire takes the sync lock: one pass at a time in this browser, across
// tabs where it has Web Locks. null: one is running.
export function acquire(): Promise<(() => void) | null> {
  if (typeof navigator === "undefined" || !("locks" in navigator) || !navigator.locks) {
    if (busyHere) return Promise.resolve(null);
    busyHere = true;
    return Promise.resolve(() => {
      busyHere = false;
    });
  }
  return new Promise((resolve) => {
    void navigator.locks.request(LOCK, { ifAvailable: true }, (lock) => {
      if (!lock) {
        resolve(null);
        return undefined;
      }
      // Held until released: the pass spans the user's confirmation.
      return new Promise<void>((release) => resolve(release));
    });
  });
}

// ensurePermission asks for read-write access to the folder if the
// browser doesn't remember it; call it straight from a click, which a
// prompt needs.
export async function ensurePermission(handle: DirHandle): Promise<boolean> {
  const mode: PermissionMode = { mode: "readwrite" };
  if (!handle.queryPermission) return true;
  if ((await handle.queryPermission(mode)) === "granted") return true;
  if (!handle.requestPermission) return false;
  return (await handle.requestPermission(mode)) === "granted";
}
