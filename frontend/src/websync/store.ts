import { isArrayOf, isNumber, isRecord, isString } from "../guards";
import { isDirHandle, type DirHandle } from "./localfs";
import type { BaseItem } from "./plan";
import { isRule, type Rule } from "./selection";

// What the browser keeps between passes, in IndexedDB: per user, the
// chosen folder's handle and the selection; per folder, base — what both
// sides last agreed on, row by row, so a pass writes only what it changed.

export type Folder = {
  handle: DirHandle;
  name: string;
  // Names this folder's base: a new folder starts from an empty one.
  id: string;
};

export type Config = {
  userId: string;
  folder?: Folder;
  rules: Rule[];
  // ISO time of the last finished pass.
  lastSync?: string;
};

export interface SyncStore {
  config(userId: string): Promise<Config>;
  saveConfig(config: Config): Promise<void>;
  base(folderId: string): Promise<BaseItem[]>;
  writeBase(folderId: string, put: BaseItem[], remove: string[]): Promise<void>;
  clearBase(folderId: string): Promise<void>;
}

function isFolder(v: unknown): v is Folder {
  return isRecord(v) && isDirHandle(v.handle) && isString(v.name) && isString(v.id);
}

function isConfig(v: unknown): v is Config {
  return (
    isRecord(v) &&
    isString(v.userId) &&
    (v.folder === undefined || isFolder(v.folder)) &&
    isArrayOf(v.rules, isRule) &&
    (v.lastSync === undefined || isString(v.lastSync))
  );
}

export function isBaseItem(v: unknown): v is BaseItem {
  return (
    isRecord(v) &&
    isString(v.key) &&
    isString(v.path) &&
    isString(v.nodeId) &&
    (v.kind === "file" || v.kind === "dir") &&
    isString(v.etag) &&
    (v.sha256 === undefined || isString(v.sha256)) &&
    isNumber(v.size) &&
    isNumber(v.lastModified)
  );
}

// In memory: tests, and a browser whose IndexedDB is off (private mode in
// some browsers) — then every pass is a first one.
export class MemoryStore implements SyncStore {
  readonly configs = new Map<string, Config>();
  readonly bases = new Map<string, Map<string, BaseItem>>();

  async config(userId: string): Promise<Config> {
    return this.configs.get(userId) ?? { userId, rules: [] };
  }

  async saveConfig(config: Config): Promise<void> {
    this.configs.set(config.userId, config);
  }

  async base(folderId: string): Promise<BaseItem[]> {
    return [...(this.bases.get(folderId)?.values() ?? [])];
  }

  async writeBase(folderId: string, put: BaseItem[], remove: string[]): Promise<void> {
    let m = this.bases.get(folderId);
    if (!m) {
      m = new Map();
      this.bases.set(folderId, m);
    }
    for (const k of remove) m.delete(k);
    for (const b of put) m.set(b.key, b);
  }

  async clearBase(folderId: string): Promise<void> {
    this.bases.delete(folderId);
  }
}

const DB_NAME = "cloud-websync";
const CONFIG = "config";
const BASE = "base";

function req<T>(r: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    r.onsuccess = () => resolve(r.result);
    r.onerror = () => reject(r.error);
  });
}

function done(tx: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error ?? new DOMException("transaction aborted", "AbortError"));
  });
}

// A base row's key: [folder id, path key]. Every string key of a folder
// sorts between [id, ""] and [id, []] — arrays come after strings in
// IndexedDB's order — so one range is the whole folder.
function folderRange(folderId: string): IDBKeyRange {
  return IDBKeyRange.bound([folderId, ""], [folderId, []]);
}

export class IdbStore implements SyncStore {
  private db?: Promise<IDBDatabase>;

  private open(): Promise<IDBDatabase> {
    this.db ??= new Promise((resolve, reject) => {
      const r = indexedDB.open(DB_NAME, 1);
      r.onupgradeneeded = () => {
        r.result.createObjectStore(CONFIG, { keyPath: "userId" });
        r.result.createObjectStore(BASE);
      };
      r.onsuccess = () => resolve(r.result);
      r.onerror = () => reject(r.error);
    });
    return this.db;
  }

  async config(userId: string): Promise<Config> {
    const db = await this.open();
    const v: unknown = await req(db.transaction(CONFIG).objectStore(CONFIG).get(userId));
    return isConfig(v) ? v : { userId, rules: [] };
  }

  async saveConfig(config: Config): Promise<void> {
    const db = await this.open();
    const tx = db.transaction(CONFIG, "readwrite");
    tx.objectStore(CONFIG).put(config);
    await done(tx);
  }

  async base(folderId: string): Promise<BaseItem[]> {
    const db = await this.open();
    const rows: unknown[] = await req(db.transaction(BASE).objectStore(BASE).getAll(folderRange(folderId)));
    return rows.filter(isBaseItem);
  }

  async writeBase(folderId: string, put: BaseItem[], remove: string[]): Promise<void> {
    if (put.length === 0 && remove.length === 0) return;
    const db = await this.open();
    const tx = db.transaction(BASE, "readwrite");
    const st = tx.objectStore(BASE);
    for (const k of remove) st.delete([folderId, k]);
    for (const b of put) st.put(b, [folderId, b.key]);
    await done(tx);
  }

  async clearBase(folderId: string): Promise<void> {
    const db = await this.open();
    const tx = db.transaction(BASE, "readwrite");
    tx.objectStore(BASE).delete(folderRange(folderId));
    await done(tx);
  }
}
