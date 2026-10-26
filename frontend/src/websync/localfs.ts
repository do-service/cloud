import { isRecord } from "../guards";
import { HASH_CHUNK, Sha256 } from "./sha256";
import { baseName, dirOf, isIgnored, isJunk, join, nameKey } from "./names";
import { sameStamp, type LocalItem, type Problem, type ProblemKind, type Stamp } from "./plan";

// The local folder through the File System Access API, behind interfaces
// a real FileSystemDirectoryHandle satisfies as it is — and an in-memory
// fake in tests (OPFS, navigator.storage.getDirectory(), too).

export interface Writable {
  write(data: Uint8Array<ArrayBuffer>): Promise<void>;
  close(): Promise<void>;
  abort(reason?: unknown): Promise<void>;
}

export interface FileHandle {
  readonly kind: "file";
  readonly name: string;
  getFile(): Promise<File>;
  createWritable(): Promise<Writable>;
  // Chromium has it for some file systems only (OPFS first): used where it
  // works, a copy where it doesn't.
  move?(destination: DirHandle, name: string): Promise<void>;
}

export type PermissionMode = { mode?: "read" | "readwrite" };

export interface DirHandle {
  readonly kind: "directory";
  readonly name: string;
  values(): AsyncIterable<DirHandle | FileHandle>;
  getDirectoryHandle(name: string, options?: { create?: boolean }): Promise<DirHandle>;
  getFileHandle(name: string, options?: { create?: boolean }): Promise<FileHandle>;
  removeEntry(name: string, options?: { recursive?: boolean }): Promise<void>;
  isSameEntry(other: DirHandle): Promise<boolean>;
  queryPermission?(descriptor?: PermissionMode): Promise<PermissionState>;
  requestPermission?(descriptor?: PermissionMode): Promise<PermissionState>;
}

// isDirHandle narrows what IndexedDB gives back to a folder handle.
export function isDirHandle(v: unknown): v is DirHandle {
  return (
    isRecord(v) &&
    v.kind === "directory" &&
    typeof v.name === "string" &&
    typeof v.getDirectoryHandle === "function" &&
    typeof v.getFileHandle === "function" &&
    typeof v.removeEntry === "function"
  );
}

// A failure the user sees as a problem with one item.
export class ItemError extends Error {
  constructor(
    readonly kind: ProblemKind,
    message: string,
    readonly code?: string,
  ) {
    super(message);
    this.name = "ItemError";
  }
}

function domName(err: unknown): string {
  return isRecord(err) && typeof err.name === "string" ? err.name : "";
}

export function isNotFound(err: unknown): boolean {
  return domName(err) === "NotFoundError";
}

// The browser refused the folder: permission withdrawn, or (on some
// systems) the folder is gone — the pass can't go on.
export function isPermissionError(err: unknown): boolean {
  return domName(err) === "NotAllowedError" || domName(err) === "SecurityError";
}

function parts(path: string): string[] {
  return path === "" ? [] : path.split("/");
}

export async function dirAt(root: DirHandle, path: string, create = false): Promise<DirHandle> {
  let d = root;
  for (const p of parts(path)) d = await d.getDirectoryHandle(p, { create });
  return d;
}

// What the scan keeps: folders by key and whether to descend.
export type ScanFilter = {
  // In the selected area: recorded, and its contents too.
  include: (key: string) => boolean;
  // A folder on the way to the area: walked, not recorded.
  descend: (key: string) => boolean;
};

export type Scan = { items: LocalItem[]; problems: Problem[] };

// scan walks the folder: size and modification time of every file in the
// area, without reading any. Names that are one to the server (a.txt and
// A.txt on a case-sensitive disk) can't both sync: the second is left out
// and reported.
export async function scan(root: DirHandle, filter: ScanFilter, signal?: AbortSignal, onItem?: (n: number) => void): Promise<Scan> {
  const items: LocalItem[] = [];
  const problems: Problem[] = [];
  const walk = async (dir: DirHandle, path: string, key: string): Promise<void> => {
    signal?.throwIfAborted();
    const children: (DirHandle | FileHandle)[] = [];
    try {
      for await (const h of dir.values()) children.push(h);
    } catch (err) {
      // Removed while being walked: as if it weren't there.
      if (isNotFound(err)) return;
      throw err;
    }
    children.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    const seen = new Set<string>();
    for (const h of children) {
      if (isIgnored(h.name)) continue;
      const k = join(key, nameKey(h.name));
      const p = join(path, h.name);
      const wanted = filter.include(k);
      if (!wanted && !(h.kind === "directory" && filter.descend(k))) continue;
      if (seen.has(k)) {
        problems.push({ kind: "localCollision", path: p });
        continue;
      }
      seen.add(k);
      if (h.kind === "directory") {
        if (wanted) items.push({ key: k, path: p, kind: "dir", size: 0, lastModified: 0 });
        await walk(h, p, k);
      } else if (wanted) {
        let f: File;
        try {
          f = await h.getFile();
        } catch (err) {
          if (isNotFound(err)) continue;
          throw err;
        }
        items.push({ key: k, path: p, kind: "file", size: f.size, lastModified: f.lastModified });
      }
      onItem?.(items.length);
    }
  };
  await walk(root, "", "");
  return { items, problems };
}

export async function fileAt(root: DirHandle, path: string): Promise<FileHandle> {
  return (await dirAt(root, dirOf(path))).getFileHandle(baseName(path));
}

// readFile is the file at path, if it still has the stamp the plan saw.
export async function readFile(root: DirHandle, path: string, expect: Stamp): Promise<File> {
  let f: File;
  try {
    f = await (await fileAt(root, path)).getFile();
  } catch (err) {
    if (isNotFound(err)) throw new ItemError("changedMeanwhile", `${path} is gone`);
    throw err;
  }
  if (!sameStamp(f, expect)) throw new ItemError("changedMeanwhile", `${path} changed since the scan`);
  return f;
}

export type Written = Stamp & { sha256: string };

// writeFile streams body into the file at path. The File System Access API
// writes through createWritable() to a swap file that replaces the file
// only on close() — abort() leaves it as it was — so an interrupted or
// corrupt download never shows under the real name; a file this call
// created is removed again. The content is hashed on the way and checked
// against sha256 and size before close(). expect is what must be at path
// now: a file with that stamp, or (undefined) nothing, so something the
// user made or changed since the scan is never overwritten.
export async function writeFile(
  root: DirHandle,
  path: string,
  body: ReadableStream<Uint8Array<ArrayBuffer>>,
  want: { size: number; sha256?: string; expect?: Stamp },
  signal?: AbortSignal,
  onBytes?: (n: number) => void,
): Promise<Written> {
  const dir = await dirAt(root, dirOf(path), true);
  const name = baseName(path);
  let fh: FileHandle | undefined;
  try {
    fh = await dir.getFileHandle(name);
  } catch (err) {
    if (!isNotFound(err)) throw err;
  }
  if (fh) {
    const now = await fh.getFile();
    if (!want.expect || !sameStamp(now, want.expect)) throw new ItemError("changedMeanwhile", `${path} changed since the scan`);
  }
  const created = !fh;
  if (!fh) {
    try {
      fh = await dir.getFileHandle(name, { create: true });
    } catch (err) {
      // The browser or the disk refuses the name.
      if (err instanceof TypeError) throw new ItemError("nameNotAllowed", `${path}: ${String(err)}`);
      throw err;
    }
  }
  const w = await fh.createWritable();
  const h = new Sha256();
  let n = 0;
  const reader = body.getReader();
  try {
    for (;;) {
      signal?.throwIfAborted();
      const { done, value } = await reader.read();
      if (done) break;
      h.update(value);
      await w.write(value);
      n += value.length;
      onBytes?.(value.length);
    }
    const sum = h.hex();
    if (n !== want.size || (want.sha256 !== undefined && sum !== want.sha256)) {
      throw new ItemError("changedMeanwhile", `${path}: got ${n} bytes with another checksum than the tree's`);
    }
    await w.close();
    const f = await fh.getFile();
    if (f.size !== want.size) throw new ItemError("failed", `${path}: ${f.size} bytes on disk, ${want.size} expected`);
    return { size: f.size, lastModified: f.lastModified, sha256: sum };
  } catch (err) {
    await reader.cancel().catch(() => undefined);
    await w.abort().catch(() => undefined);
    if (created) await dir.removeEntry(name).catch(() => undefined);
    throw err;
  }
}

// removeFile deletes the file at path if it still has the plan's stamp.
export async function removeFile(root: DirHandle, path: string, expect?: Stamp): Promise<void> {
  let dir: DirHandle;
  let f: File;
  try {
    dir = await dirAt(root, dirOf(path));
    f = await (await dir.getFileHandle(baseName(path))).getFile();
  } catch (err) {
    if (isNotFound(err)) return; // already gone
    throw err;
  }
  if (expect && !sameStamp(f, expect)) throw new ItemError("changedMeanwhile", `${path} changed since the scan`);
  await dir.removeEntry(baseName(path));
}

// removeDirIfEmpty deletes an empty folder; one with anything in it (the
// user's, or what this pass kept) stays, and false says so.
export async function removeDirIfEmpty(root: DirHandle, path: string): Promise<boolean> {
  let dir: DirHandle;
  try {
    dir = await dirAt(root, dirOf(path));
    const target = await dir.getDirectoryHandle(baseName(path));
    for await (const h of target.values()) {
      // OS junk doesn't keep a folder: it goes with it. A lock file does:
      // a document in there is open.
      if (!isJunk(h.name)) return false;
    }
  } catch (err) {
    if (isNotFound(err)) return true;
    throw err;
  }
  await dir.removeEntry(baseName(path), { recursive: true });
  return true;
}

// moveFile renames or moves a file within the folder (expect: its stamp
// now). With FileSystemHandle.move where the browser has it for this file
// system, else by copying and removing the original. The result is the
// file's stamp at its new place.
export async function moveFile(root: DirHandle, from: string, to: string, expect: Stamp): Promise<Stamp> {
  const src = await fileAt(root, from).catch((err: unknown) => {
    if (isNotFound(err)) throw new ItemError("changedMeanwhile", `${from} is gone`);
    throw err;
  });
  const file = await src.getFile();
  if (!sameStamp(file, expect)) throw new ItemError("changedMeanwhile", `${from} changed since the scan`);
  const destDir = await dirAt(root, dirOf(to), true);
  try {
    await destDir.getFileHandle(baseName(to));
    throw new ItemError("changedMeanwhile", `${to} is taken`);
  } catch (err) {
    if (!isNotFound(err)) throw err;
  }
  if (src.move) {
    try {
      await src.move(destDir, baseName(to));
      const f = await (await destDir.getFileHandle(baseName(to))).getFile();
      return { size: f.size, lastModified: f.lastModified };
    } catch (err) {
      // Not for this file system: copied below.
      if (domName(err) !== "NotSupportedError" && domName(err) !== "InvalidModificationError" && !(err instanceof TypeError)) throw err;
    }
  }
  const written = await writeFile(root, to, blobStream(file), { size: file.size });
  await removeFile(root, from, expect);
  return written;
}

// blobStream reads a file in slices, as hashFile does.
function blobStream(b: Blob): ReadableStream<Uint8Array<ArrayBuffer>> {
  let off = 0;
  return new ReadableStream({
    async pull(ctl) {
      if (off >= b.size) {
        ctl.close();
        return;
      }
      const chunk = new Uint8Array(await b.slice(off, off + HASH_CHUNK).arrayBuffer());
      off += chunk.length;
      ctl.enqueue(chunk);
    },
  });
}
