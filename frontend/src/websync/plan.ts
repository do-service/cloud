import { baseName, conflictName, depth, dirOf, join, nameKey, under } from "./names";

// The browser's two-way folder sync plans a pass as the desktop client's
// engine does (docs/desktop-client.md, «Сверка трёх состояний»): every
// path is looked at in three states — base (what both sides last agreed
// on, kept in IndexedDB), local (the folder now) and remote (the server's
// tree now) — and the difference decides. Unlike the desktop, a browser
// has no file ids and no watcher: a local move is a delete plus a new file
// (told apart by content, below), and a pass runs when the user asks.
//
// Paths are relative to the chosen folder; a key is the path's name keys
// (names.ts), so "Docs/A.txt" and "docs/a.txt" are one entry, as they are
// to the server and to a case-insensitive disk.

export type Kind = "file" | "dir";

// What the disk says about a file without reading it.
export type Stamp = { size: number; lastModified: number };

export type RemoteItem = {
  key: string;
  // With the server's names.
  path: string;
  id: string;
  kind: Kind;
  size: number;
  etag: string;
  sha256?: string;
  // The user may change it (a file's content) or add to it (a folder).
  writable: boolean;
  // The user may delete it or move it away from its folder.
  parentWritable: boolean;
  // «Доступно мне» and the shares in it: the user can't delete or move
  // them on the server, so deleting one locally brings it back.
  pinned: boolean;
  // A folder on the way to a selected one: made on disk, otherwise not
  // synced — what else is in it stays on its side.
  container: boolean;
  // The tree it is in (the own root or a share): nothing moves between
  // trees on the server.
  root: string;
};

export type LocalItem = {
  key: string;
  // With the names on disk.
  path: string;
  kind: Kind;
  size: number;
  lastModified: number;
  // Computed only where needed (needsHash).
  sha256?: string;
};

export type BaseItem = {
  key: string;
  path: string;
  nodeId: string;
  kind: Kind;
  etag: string;
  sha256?: string;
  size: number;
  lastModified: number;
};

// The selected part of the tree, by key: the nearest of include and
// exclude above a path decides, none is outside.
export type Area = { include: string[]; exclude: string[] };

export type Snapshot = {
  remote: RemoteItem[];
  local: LocalItem[];
  base: BaseItem[];
  area: Area;
  // The «(конфликт, Устройство, дата)» part of a conflict copy's name, nth
  // try: the caller has the dictionary and the clock.
  conflictTag: (n: number) => string;
};

export type ProblemKind =
  | "readOnlyEdit"
  | "readOnlyNew"
  | "keptLocally"
  | "localCollision"
  | "typeMismatch"
  | "nameNotAllowed"
  | "reservedName"
  | "precondition"
  | "quota"
  | "locked"
  | "nameConflict"
  | "forbidden"
  | "changedMeanwhile"
  | "folderNotEmpty"
  | "failed";

export type Problem = { kind: ProblemKind; path: string; code?: string };

// The local state a write expects to find, so a file changed since the
// scan is never overwritten or removed: the next pass sees the change.
type Expect = { expect?: Stamp };

export type Op =
  | { type: "mkdirLocal"; key: string; path: string; nodeId: string; container: boolean }
  | { type: "mkdirRemote"; key: string; path: string; parentKey: string; name: string }
  | ({ type: "download"; key: string; path: string; nodeId: string; etag: string; sha256?: string; size: number } & Expect)
  | {
      type: "upload";
      key: string;
      path: string;
      parentKey: string;
      name: string;
      // A new version of this file, if it was based on ifMatch.
      nodeId?: string;
      ifMatch?: string;
      size: number;
      sha256?: string;
      expect: Stamp;
    }
  | {
      // The local edit steps aside under copyPath (and goes up as a new
      // file if upload), and the server's version comes down to path.
      type: "conflict";
      key: string;
      path: string;
      copyKey: string;
      copyPath: string;
      parentKey: string;
      upload: boolean;
      expect: Stamp;
      nodeId: string;
      etag: string;
      sha256?: string;
      size: number;
    }
  // A move moves base's row along when it is made.
  | { type: "moveLocal"; key: string; path: string; fromKey: string; fromPath: string; nodeId: string; etag: string; sha256?: string; expect: Stamp }
  | {
      type: "moveRemote";
      key: string;
      path: string;
      fromKey: string;
      nodeId: string;
      parentKey: string;
      name: string;
      ifMatch: string;
      sha256?: string;
      stamp: Stamp;
    }
  | { type: "setBase"; base: BaseItem }
  | { type: "forget"; key: string }
  // Deletes: to the trash on the server (If-Match for a file), or from
  // disk. confirm marks a delete the user must agree to first; one that
  // only clears an emptied folder or what moved elsewhere needs none.
  // baseKey: base's row it settles, when not key's.
  | { type: "trashRemote"; key: string; path: string; nodeId: string; kind: Kind; ifMatch?: string; baseKey?: string; confirm: true }
  | ({ type: "removeLocal"; key: string; path: string; kind: Kind; nodeId: string; confirm: boolean } & Expect);

export type Plan = { ops: Op[]; problems: Problem[] };

export function sameStamp(a: Stamp, b: Stamp): boolean {
  return a.size === b.size && a.lastModified === b.lastModified;
}

export function inArea(area: Area, key: string): boolean {
  let best = -1;
  let included = false;
  for (const k of area.include) {
    if (under(key, k) && depth(k) > best) {
      best = depth(k);
      included = true;
    }
  }
  for (const k of area.exclude) {
    if (under(key, k) && depth(k) >= best) {
      best = depth(k);
      included = false;
    }
  }
  return included;
}

// needsHash lists the local files whose content must be known to plan:
// those base doesn't know (new, or met on a first pass) and those whose
// size or modification time moved since base — a touch isn't an edit.
export function needsHash(local: LocalItem[], base: BaseItem[], area: Area): LocalItem[] {
  const byKey = new Map(base.map((b) => [b.key, b]));
  return local.filter((l) => {
    if (l.kind !== "file" || l.sha256 !== undefined || !inArea(area, l.key)) return false;
    const b = byKey.get(l.key);
    return !b || b.kind !== "file" || !sameStamp(l, b);
  });
}

// Ops run in phases (execute.ts): folders top-down, moves, transfers,
// then deletes bottom-up.
export function opPhase(op: Op): number {
  switch (op.type) {
    case "mkdirLocal":
    case "mkdirRemote":
      return 0;
    case "moveLocal":
    case "moveRemote":
      return 1;
    case "download":
    case "upload":
    case "conflict":
    case "setBase":
    case "forget":
      return 2;
    case "trashRemote":
    case "removeLocal":
      return 3;
  }
}

export function opKey(op: Op): string {
  return op.type === "setBase" ? op.base.key : op.key;
}

export function makePlan(s: Snapshot): Plan {
  return new Planner(s).run();
}

class Planner {
  private readonly R = new Map<string, RemoteItem>();
  private readonly byId = new Map<string, RemoteItem>();
  private readonly L = new Map<string, LocalItem>();
  private readonly B = new Map<string, BaseItem>();
  // Names (keys) of what a folder (by key) holds on either side or gets in
  // this pass: a conflict copy's name must be free of all of them.
  private readonly names = new Map<string, Set<string>>();
  private readonly ops: Op[] = [];
  private readonly problems: Problem[] = [];
  // Keys this pass has decided.
  private readonly done = new Set<string>();
  // Folders whose subtree is left alone (a problem was reported for it).
  private readonly blocked: string[] = [];
  // Folders made on the server or on disk in this pass.
  private readonly mkRemote = new Set<string>();
  private readonly mkLocal = new Set<string>();
  // Folders something comes down into in this pass.
  private readonly keepLocal = new Set<string>();
  // Memo of remoteParentOk.
  private readonly parentOk = new Map<string, boolean>();

  constructor(private readonly s: Snapshot) {
    for (const r of s.remote) {
      this.R.set(r.key, r);
      this.byId.set(r.id, r);
      this.name(r.key);
    }
    for (const l of s.local) {
      this.L.set(l.key, l);
      this.name(l.key);
    }
    for (const b of s.base) this.B.set(b.key, b);
  }

  run(): Plan {
    const keys = new Set<string>();
    for (const k of [...this.R.keys(), ...this.L.keys(), ...this.B.keys()]) if (k !== "") keys.add(k);
    const sorted = [...keys].sort((a, b) => depth(a) - depth(b) || (a < b ? -1 : a > b ? 1 : 0));

    for (const k of sorted) {
      const b = this.B.get(k);
      if (b && !this.area(k)) {
        // Unchecked: no longer synced, its files stay where they are.
        this.add({ type: "forget", key: k });
        this.done.add(k);
      }
    }
    for (const k of sorted) this.containers(k);
    for (const k of sorted) this.kinds(k);
    this.remoteMoves(sorted);
    this.localMoves(sorted);
    for (const k of sorted) this.newDirs(k);
    for (const k of sorted) this.file(k);
    for (const k of [...sorted].reverse()) this.dir(k);
    return { ops: this.sortOps(), problems: this.problems };
  }

  private add(op: Op) {
    this.ops.push(op);
    if (op.type === "download" || op.type === "mkdirLocal" || op.type === "moveLocal") {
      // Its folders stay on disk (dir), whatever else happens to them.
      for (let d = dirOf(op.key); d !== ""; d = dirOf(d)) {
        if (this.keepLocal.has(d)) break;
        this.keepLocal.add(d);
      }
    }
  }

  private problem(kind: ProblemKind, path: string) {
    this.problems.push({ kind, path });
  }

  private area(key: string): boolean {
    return inArea(this.s.area, key);
  }

  private name(key: string) {
    const d = dirOf(key);
    let set = this.names.get(d);
    if (!set) {
      set = new Set();
      this.names.set(d, set);
    }
    set.add(baseName(key));
  }

  private isBlocked(key: string): boolean {
    return this.blocked.some((b) => under(key, b));
  }

  private block(key: string) {
    this.blocked.push(key);
  }

  // Where key is on disk, or goes: the disk's names for the folders that
  // exist, so a case-sensitive disk doesn't get a second "Docs" next to
  // "docs".
  private localPath(key: string, remoteName: string): string {
    const l = this.L.get(key);
    if (l) return l.path;
    const d = dirOf(key);
    return join(d === "" ? "" : this.localPath(d, baseName(this.R.get(d)?.path ?? this.B.get(d)?.path ?? d)), remoteName);
  }

  private remoteName(r: RemoteItem): string {
    return baseName(r.path);
  }

  private changedLocally(l: LocalItem, b: BaseItem): boolean {
    if (sameStamp(l, b)) return false;
    // Not hashed (it couldn't be read): taken for an edit, which at worst
    // uploads the same bytes again.
    return l.sha256 === undefined || b.sha256 === undefined || l.sha256 !== b.sha256;
  }

  private changedRemotely(r: RemoteItem, b: BaseItem): boolean {
    // A new ETag with the same SHA-256 (the same bytes uploaded again) is
    // no change.
    return r.etag !== b.etag && (r.sha256 === undefined || b.sha256 === undefined || r.sha256 !== b.sha256);
  }

  private setBase(l: LocalItem, r: RemoteItem) {
    this.add({
      type: "setBase",
      base: { key: l.key, path: l.path, nodeId: r.id, kind: r.kind, etag: r.etag, sha256: r.sha256 ?? l.sha256, size: l.size, lastModified: l.lastModified },
    });
  }

  private download(key: string, r: RemoteItem, expect?: Stamp) {
    const path = this.localPath(key, this.remoteName(r));
    this.add({ type: "download", key, path, nodeId: r.id, etag: r.etag, sha256: r.sha256, size: r.size, expect });
  }

  // A conflict copy's name next to key, free on both sides and in this
  // pass.
  private copyName(key: string, name: string): string {
    const d = dirOf(key);
    const taken = this.names.get(d) ?? new Set<string>();
    for (let n = 1; ; n++) {
      const c = conflictName(name, this.s.conflictTag(n));
      if (!taken.has(nameKey(c))) {
        this.name(join(d, nameKey(c)));
        return c;
      }
    }
  }

  private conflict(l: LocalItem, r: RemoteItem) {
    const copy = this.copyName(l.key, baseName(l.path));
    const copyKey = join(dirOf(l.key), nameKey(copy));
    const upload = r.parentWritable && !(r.pinned && r.kind === "file");
    this.add({
      type: "conflict",
      key: l.key,
      path: l.path,
      copyKey,
      copyPath: join(dirOf(l.path), copy),
      parentKey: dirOf(l.key),
      upload,
      expect: { size: l.size, lastModified: l.lastModified },
      nodeId: r.id,
      etag: r.etag,
      sha256: r.sha256,
      size: r.size,
    });
    // The copy stays on disk, unsynced, where the user can't upload to.
    if (!upload) this.problem("readOnlyEdit", join(dirOf(l.path), copy));
  }

  private uploadNew(l: LocalItem) {
    this.add({
      type: "upload",
      key: l.key,
      path: l.path,
      parentKey: dirOf(l.key),
      name: baseName(l.path),
      size: l.size,
      sha256: l.sha256,
      expect: { size: l.size, lastModified: l.lastModified },
    });
  }

  // remoteParentOk: a new local file or folder in dirKey can go up — the
  // folder is on the server and writable, or is made there in this pass
  // (recreated, if it was deleted there while the user added to it).
  private remoteParentOk(dirKey: string): boolean {
    const memo = this.parentOk.get(dirKey);
    if (memo !== undefined) return memo;
    let ok = false;
    const r = this.R.get(dirKey);
    if (r) {
      ok = r.kind === "dir" && !r.container && r.writable && !this.isBlocked(dirKey);
    } else if (this.mkRemote.has(dirKey)) {
      ok = true;
    } else if (dirKey !== "" && this.L.get(dirKey)?.kind === "dir" && this.area(dirKey) && !this.isBlocked(dirKey)) {
      ok = this.remoteParentOk(dirOf(dirKey));
      if (ok) {
        this.mkRemote.add(dirKey);
        this.done.add(dirKey);
        const l = this.L.get(dirKey);
        if (l) this.add({ type: "mkdirRemote", key: dirKey, path: l.path, parentKey: dirOf(dirKey), name: baseName(l.path) });
      }
    }
    this.parentOk.set(dirKey, ok);
    return ok;
  }

  // Containers are made on disk and never synced themselves.
  private containers(k: string) {
    const r = this.R.get(k);
    if (!r?.container) return;
    this.done.add(k);
    const l = this.L.get(k);
    if (l && l.kind !== "dir") {
      this.problem("typeMismatch", l.path);
      this.block(k);
    } else if (!l) {
      this.mkLocal.add(k);
      this.add({ type: "mkdirLocal", key: k, path: this.localPath(k, this.remoteName(r)), nodeId: r.id, container: true });
    }
  }

  // A file on one side and a folder on the other can't be reconciled: the
  // user sorts it out. A side that replaced base's entry with one of the
  // other kind just makes base's row stale.
  private kinds(k: string) {
    if (this.done.has(k) || this.isBlocked(k) || !this.area(k)) return;
    const l = this.L.get(k);
    const r = this.R.get(k);
    const b = this.B.get(k);
    if (b && ((r && r.kind !== b.kind) || (!r && l && l.kind !== b.kind))) {
      this.add({ type: "forget", key: k });
      this.B.delete(k);
    }
    if (l && r && l.kind !== r.kind) {
      this.problem("typeMismatch", l.path);
      this.block(k);
      this.done.add(k);
    }
  }

  // A file the server moved or renamed: base knows it at one key, the
  // tree has it at another.
  private remoteMoves(sorted: string[]) {
    for (const k of sorted) {
      const b = this.B.get(k);
      if (!b || b.kind !== "file" || this.done.has(k) || !this.area(k) || this.isBlocked(k)) continue;
      const r = this.byId.get(b.nodeId);
      if (!r || r.key === k || r.container || !this.area(r.key) || this.isBlocked(r.key) || this.done.has(r.key)) continue;
      const l = this.L.get(k);
      const there = this.L.get(r.key);
      const rc = this.changedRemotely(r, b);
      if (!l) {
        if (there || this.B.get(r.key)) continue; // left to the per-key rules
        // Deleted here, moved there: the delete follows it, unless the
        // server's side was edited too (an edit beats a delete).
        this.done.add(k).add(r.key);
        if (rc || !r.parentWritable || r.pinned) {
          this.add({ type: "forget", key: k });
          this.download(r.key, r);
        } else {
          this.add({ type: "trashRemote", key: r.key, path: r.path, nodeId: r.id, kind: "file", ifMatch: b.etag, baseKey: k, confirm: true });
        }
        continue;
      }
      if (l.kind !== "file" || there) continue;
      const lc = this.changedLocally(l, b);
      if (!lc && !rc) {
        this.done.add(k).add(r.key);
        const to = this.localPath(r.key, this.remoteName(r));
        this.name(r.key);
        this.add({
          type: "moveLocal",
          key: r.key,
          path: to,
          fromKey: k,
          fromPath: l.path,
          nodeId: r.id,
          etag: r.etag,
          sha256: r.sha256 ?? b.sha256,
          expect: { size: l.size, lastModified: l.lastModified },
        });
      } else if (lc && !rc && r.writable) {
        // The edit goes to the node now; the move comes down next pass.
        this.done.add(k).add(r.key);
        this.add({
          type: "upload",
          key: k,
          path: l.path,
          parentKey: dirOf(k),
          name: baseName(l.path),
          nodeId: b.nodeId,
          ifMatch: b.etag,
          size: l.size,
          sha256: l.sha256,
          expect: { size: l.size, lastModified: l.lastModified },
        });
      }
      // Otherwise both changed, or it is read-only: the per-key rules make
      // the server's version come down at its new place and the edit a
      // conflict copy at the old one.
    }
  }

  // A file moved or renamed locally shows as one gone from base's place
  // and a new one elsewhere; the same size and SHA-256, one to one, make
  // it a move on the server instead of an upload and a delete.
  private localMoves(sorted: string[]) {
    const gone = new Map<string, BaseItem[]>();
    const fresh = new Map<string, LocalItem[]>();
    for (const k of sorted) {
      if (this.done.has(k) || !this.area(k) || this.isBlocked(k)) continue;
      const b = this.B.get(k);
      const l = this.L.get(k);
      const r = this.R.get(k);
      if (b?.kind === "file" && b.sha256 && !l && r && r.id === b.nodeId && !this.changedRemotely(r, b) && r.parentWritable && !r.pinned) {
        const sig = `${b.size}:${b.sha256}`;
        gone.set(sig, [...(gone.get(sig) ?? []), b]);
      }
      if (l?.kind === "file" && l.sha256 && !b && !r) {
        const sig = `${l.size}:${l.sha256}`;
        fresh.set(sig, [...(fresh.get(sig) ?? []), l]);
      }
    }
    for (const [sig, bs] of gone) {
      const ls = fresh.get(sig);
      if (bs.length !== 1 || ls?.length !== 1) continue;
      const [b] = bs;
      const [l] = ls;
      const r = this.R.get(b.key);
      if (!r) continue;
      const to = dirOf(l.key);
      // Within its tree, into a folder that takes it.
      const dest = this.R.get(to);
      if (!this.remoteParentOk(to) || (dest && dest.root !== r.root) || (!dest && this.rootOfKey(to) !== r.root)) continue;
      this.done.add(b.key).add(l.key);
      this.add({
        type: "moveRemote",
        key: l.key,
        path: l.path,
        fromKey: b.key,
        nodeId: b.nodeId,
        parentKey: to,
        name: baseName(l.path),
        ifMatch: b.etag,
        sha256: b.sha256,
        stamp: { size: l.size, lastModified: l.lastModified },
      });
    }
  }

  // The tree a folder that isn't on the server yet goes into: that of the
  // nearest one above it that is.
  private rootOfKey(key: string): string | undefined {
    for (let k = key; ; k = dirOf(k)) {
      const r = this.R.get(k);
      if (r) return r.root;
      if (k === "") return undefined;
    }
  }

  // Folders new on one side are made on the other first, top-down, so
  // what's in them has somewhere to go.
  private newDirs(k: string) {
    if (this.done.has(k) || !this.area(k) || this.isBlocked(k)) return;
    const l = this.L.get(k);
    const r = this.R.get(k);
    const b = this.B.get(k);
    if (l?.kind === "dir" && r?.kind === "dir") {
      this.done.add(k);
      if (!b || b.nodeId !== r.id || b.kind !== "dir") {
        this.add({ type: "setBase", base: { key: k, path: l.path, nodeId: r.id, kind: "dir", etag: r.etag, size: 0, lastModified: 0 } });
      }
    } else if (l?.kind === "dir" && !r && !b) {
      this.done.add(k);
      if (!this.remoteParentOk(dirOf(k))) {
        this.problem("readOnlyNew", l.path);
        this.block(k);
        return;
      }
      this.mkRemote.add(k);
      this.parentOk.set(k, true);
      this.add({ type: "mkdirRemote", key: k, path: l.path, parentKey: dirOf(k), name: baseName(l.path) });
    } else if (r?.kind === "dir" && !l && (!b || b.nodeId !== r.id)) {
      this.done.add(k);
      this.mkLocal.add(k);
      this.add({ type: "mkdirLocal", key: k, path: this.localPath(k, this.remoteName(r)), nodeId: r.id, container: false });
    }
  }

  private file(k: string) {
    if (this.done.has(k) || !this.area(k) || this.isBlocked(k)) return;
    const l = this.L.get(k);
    const r = this.R.get(k);
    const b = this.B.get(k);
    if ((l ?? r ?? b)?.kind !== "file") return;
    this.done.add(k);
    const stamp = l && { size: l.size, lastModified: l.lastModified };

    if (b && l && r && r.id === b.nodeId) {
      const lc = this.changedLocally(l, b);
      const rc = this.changedRemotely(r, b);
      if (!lc && !rc) {
        if (!sameStamp(l, b) || r.etag !== b.etag) this.setBase(l, r);
      } else if (lc && !rc) {
        if (r.writable) {
          this.add({
            type: "upload",
            key: k,
            path: l.path,
            parentKey: dirOf(k),
            name: baseName(l.path),
            nodeId: r.id,
            ifMatch: b.etag,
            size: l.size,
            sha256: l.sha256,
            expect: { size: l.size, lastModified: l.lastModified },
          });
        } else {
          // Read-only: the edit is kept aside, the server's version comes
          // back under the name.
          this.conflict(l, { ...r, parentWritable: false });
        }
      } else if (!lc && rc) {
        this.download(k, r, stamp);
      } else if (l.sha256 !== undefined && l.sha256 === r.sha256) {
        this.setBase(l, r);
      } else {
        this.conflict(l, r);
      }
      return;
    }

    if (b && l) {
      // Base's node isn't here on the server any more.
      const lc = this.changedLocally(l, b);
      if (r) {
        // Another file took the name there.
        if (!lc) this.download(k, r, stamp);
        else if (l.sha256 !== undefined && l.sha256 === r.sha256) this.setBase(l, r);
        else this.conflict(l, r);
        return;
      }
      const moved = this.byId.has(b.nodeId);
      if (!lc) {
        // Moved: it comes down at its new place (remoteMoves or the
        // per-key rules there), so this copy goes without asking.
        this.add({ type: "removeLocal", key: k, path: l.path, kind: "file", nodeId: b.nodeId, expect: stamp, confirm: !moved });
        return;
      }
      // An edit beats the server's delete: it goes up as a new file.
      this.add({ type: "forget", key: k });
      if (this.remoteParentOk(dirOf(k))) this.uploadNew(l);
      else this.problem("keptLocally", l.path);
      return;
    }

    if (b && r) {
      // Gone from disk.
      if (r.id !== b.nodeId || this.changedRemotely(r, b) || r.pinned || !r.parentWritable) {
        // A new file there, an edit on the server (it beats the delete),
        // or a place the user can't delete from: it comes back.
        this.download(k, r);
      } else {
        this.add({ type: "trashRemote", key: k, path: r.path, nodeId: r.id, kind: "file", ifMatch: b.etag, confirm: true });
      }
      return;
    }

    if (b) {
      this.add({ type: "forget", key: k });
      return;
    }

    if (l && r) {
      // Met on a first pass (or made on both sides since): the same bytes
      // are one file.
      if (l.sha256 !== undefined && l.sha256 === r.sha256) this.setBase(l, r);
      else this.conflict(l, r);
      return;
    }
    if (l) {
      if (this.remoteParentOk(dirOf(k))) this.uploadNew(l);
      else this.problem("readOnlyNew", l.path);
      return;
    }
    if (r) this.download(k, r);
  }

  // Folders base knows but one side lost, bottom-up: what is planned for
  // their contents decides whether they go or come back.
  private dir(k: string) {
    if (this.done.has(k) || !this.area(k) || this.isBlocked(k)) return;
    const l = this.L.get(k);
    const r = this.R.get(k);
    const b = this.B.get(k);
    if ((l ?? r ?? b)?.kind !== "dir") return;
    this.done.add(k);
    if (!b) return;
    if (!l && !r) {
      this.add({ type: "forget", key: k });
      return;
    }
    if (l && !r) {
      if (this.mkRemote.has(k)) return; // recreated for what's new in it
      // Moved on the server: only the emptied folder goes, no question.
      const moved = this.byId.has(b.nodeId);
      this.add({ type: "removeLocal", key: k, path: l.path, kind: "dir", nodeId: b.nodeId, confirm: !moved });
      return;
    }
    if (r && !l) {
      if (this.mkLocal.has(k) || this.keepLocal.has(k) || r.pinned || !r.parentWritable) {
        if (!this.mkLocal.has(k)) {
          this.mkLocal.add(k);
          this.add({ type: "mkdirLocal", key: k, path: this.localPath(k, this.remoteName(r)), nodeId: r.id, container: false });
        }
        return;
      }
      this.add({ type: "trashRemote", key: k, path: r.path, nodeId: r.id, kind: "dir", confirm: true });
    }
  }

  private sortOps(): Op[] {
    return this.ops
      .map((op, i) => ({ op, i }))
      .sort((a, b) => {
        const pa = opPhase(a.op);
        const pb = opPhase(b.op);
        if (pa !== pb) return pa - pb;
        const da = depth(opKey(a.op));
        const db = depth(opKey(b.op));
        // Folders before what's in them, but deletes from the bottom.
        if (da !== db) return pa === 3 ? db - da : da - db;
        return a.i - b.i;
      })
      .map((x) => x.op);
  }
}
