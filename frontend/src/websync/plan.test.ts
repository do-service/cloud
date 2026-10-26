import { describe, expect, it } from "vitest";
import { pathKey } from "./names";
import { inArea, makePlan, needsHash, type Area, type BaseItem, type LocalItem, type Op, type Plan, type RemoteItem, type Snapshot } from "./plan";

// The desktop client's reconciliation table (docs/desktop-client.md,
// «Сверка трёх состояний»), cell by cell, for the browser's planner.

const TAG = "(conflict, Browser, 2026-10-26 10-00)";
const tag = (n: number) => (n > 1 ? TAG.replace(")", `, ${n})`) : TAG);

type RemoteOpts = Partial<Pick<RemoteItem, "writable" | "parentWritable" | "pinned" | "container" | "root" | "size">>;

const rootDir: RemoteItem = {
  key: "",
  path: "",
  id: "root",
  kind: "dir",
  size: 0,
  etag: "e-root",
  writable: true,
  parentWritable: false,
  pinned: false,
  container: false,
  root: "root",
};

function rf(path: string, id: string, etag: string, sha: string, o: RemoteOpts = {}): RemoteItem {
  return { key: pathKey(path), path, id, kind: "file", size: o.size ?? sha.length, etag, sha256: sha, writable: true, parentWritable: true, pinned: false, container: false, root: "root", ...o };
}

function rd(path: string, id: string, o: RemoteOpts = {}): RemoteItem {
  return { key: pathKey(path), path, id, kind: "dir", size: 0, etag: `e-${id}`, writable: true, parentWritable: true, pinned: false, container: false, root: "root", ...o };
}

function lf(path: string, sha: string, lastModified = 1, size = sha.length): LocalItem {
  return { key: pathKey(path), path, kind: "file", size, lastModified, sha256: sha };
}

function ld(path: string): LocalItem {
  return { key: pathKey(path), path, kind: "dir", size: 0, lastModified: 0 };
}

function bf(path: string, nodeId: string, etag: string, sha: string, lastModified = 1, size = sha.length): BaseItem {
  return { key: pathKey(path), path, nodeId, kind: "file", etag, sha256: sha, size, lastModified };
}

function bd(path: string, nodeId: string): BaseItem {
  return { key: pathKey(path), path, nodeId, kind: "dir", etag: "", size: 0, lastModified: 0 };
}

const ALL: Area = { include: [""], exclude: [] };

function plan(p: { remote?: RemoteItem[]; local?: LocalItem[]; base?: BaseItem[]; area?: Area }): Plan {
  const s: Snapshot = { remote: [rootDir, ...(p.remote ?? [])], local: p.local ?? [], base: p.base ?? [], area: p.area ?? ALL, conflictTag: tag };
  return makePlan(s);
}

// The ops in a compact form: "type key".
function brief(ops: Op[]): string[] {
  return ops.map((o) => `${o.type} ${o.type === "setBase" ? o.base.key : o.key}`);
}

function only<T extends Op["type"]>(p: Plan, type: T): Extract<Op, { type: T }> {
  const found = p.ops.filter((o): o is Extract<Op, { type: T }> => o.type === type);
  expect(found).toHaveLength(1);
  return found[0];
}

describe("makePlan: one file, base known", () => {
  const base = [bf("a.txt", "n1", "e1", "aaa")];

  it("does nothing when neither side changed", () => {
    expect(plan({ remote: [rf("a.txt", "n1", "e1", "aaa")], local: [lf("a.txt", "aaa")], base }).ops).toEqual([]);
  });

  it("uploads a local edit as a new version, if the server still has base's", () => {
    const op = only(plan({ remote: [rf("a.txt", "n1", "e1", "aaa")], local: [lf("a.txt", "bbbb", 2)], base }), "upload");
    expect(op).toMatchObject({ nodeId: "n1", ifMatch: "e1", sha256: "bbbb", expect: { size: 4, lastModified: 2 } });
  });

  it("downloads a server edit over the unchanged local file", () => {
    const op = only(plan({ remote: [rf("a.txt", "n1", "e2", "ccc")], local: [lf("a.txt", "aaa")], base }), "download");
    expect(op).toMatchObject({ nodeId: "n1", etag: "e2", sha256: "ccc", expect: { size: 3, lastModified: 1 } });
  });

  it("only records base when both sides made the same edit", () => {
    const p = plan({ remote: [rf("a.txt", "n1", "e2", "ddd")], local: [lf("a.txt", "ddd", 5)], base });
    expect(brief(p.ops)).toEqual(["setBase a.txt"]);
    expect(only(p, "setBase").base).toMatchObject({ etag: "e2", lastModified: 5 });
  });

  it("makes a conflict copy when both sides edited differently", () => {
    const op = only(plan({ remote: [rf("a.txt", "n1", "e2", "ddd")], local: [lf("a.txt", "eee", 5)], base }), "conflict");
    expect(op).toMatchObject({ path: "a.txt", copyPath: `a ${TAG}.txt`, upload: true, nodeId: "n1", etag: "e2" });
  });

  it("takes a touch (new mtime, same bytes) for no change", () => {
    expect(brief(plan({ remote: [rf("a.txt", "n1", "e1", "aaa")], local: [lf("a.txt", "aaa", 9)], base }).ops)).toEqual(["setBase a.txt"]);
  });

  it("takes a new ETag with the same SHA-256 for no change", () => {
    expect(brief(plan({ remote: [rf("a.txt", "n1", "e9", "aaa")], local: [lf("a.txt", "aaa")], base }).ops)).toEqual(["setBase a.txt"]);
  });

  it("trashes on the server what was deleted locally, asking first", () => {
    const op = only(plan({ remote: [rf("a.txt", "n1", "e1", "aaa")], base }), "trashRemote");
    expect(op).toMatchObject({ nodeId: "n1", ifMatch: "e1", confirm: true });
  });

  it("downloads again what was deleted locally but edited on the server (edit beats delete)", () => {
    expect(brief(plan({ remote: [rf("a.txt", "n1", "e2", "zzz")], base }).ops)).toEqual(["download a.txt"]);
  });

  it("removes locally what the server deleted, asking first", () => {
    const op = only(plan({ local: [lf("a.txt", "aaa")], base }), "removeLocal");
    expect(op).toMatchObject({ confirm: true, expect: { size: 3, lastModified: 1 } });
  });

  it("uploads as new what was edited locally but deleted on the server (edit beats delete)", () => {
    const p = plan({ local: [lf("a.txt", "xyz", 3)], base });
    expect(brief(p.ops)).toEqual(["forget a.txt", "upload a.txt"]);
    expect(only(p, "upload")).toMatchObject({ parentKey: "", name: "a.txt" });
    expect(only(p, "upload").nodeId).toBeUndefined();
  });

  it("forgets what both sides deleted", () => {
    expect(brief(plan({ base }).ops)).toEqual(["forget a.txt"]);
  });

  it("downloads a new file that took the name on the server", () => {
    expect(brief(plan({ remote: [rf("a.txt", "n2", "f1", "new")], local: [lf("a.txt", "aaa")], base }).ops)).toEqual(["download a.txt"]);
  });
});

describe("makePlan: new on one or both sides", () => {
  it("downloads a new server file", () => {
    expect(only(plan({ remote: [rf("docs/a.txt", "n1", "e1", "aaa")], local: [ld("docs")], base: [bd("docs", "d1")] }), "download")).toMatchObject({
      path: "docs/a.txt",
      expect: undefined,
    });
  });

  it("uploads a new local file into its folder", () => {
    const p = plan({ remote: [rd("docs", "d1")], local: [ld("docs"), lf("docs/n.txt", "nnn")], base: [bd("docs", "d1")] });
    expect(only(p, "upload")).toMatchObject({ parentKey: "docs", name: "n.txt" });
    expect(only(p, "upload").nodeId).toBeUndefined();
  });

  it("makes a new local folder on the server before what's in it", () => {
    const p = plan({ local: [ld("New"), lf("New/x.txt", "xxx")] });
    expect(brief(p.ops)).toEqual(["mkdirRemote new", "upload new/x.txt"]);
    expect(only(p, "upload")).toMatchObject({ parentKey: "new", path: "New/x.txt" });
  });

  it("makes a new server folder on disk", () => {
    expect(brief(plan({ remote: [rd("Pics", "d1"), rf("Pics/p.jpg", "n1", "e1", "ppp")] }).ops)).toEqual(["mkdirLocal pics", "download pics/p.jpg"]);
  });

  it("takes the same file met on both sides (a first pass) for synced", () => {
    expect(brief(plan({ remote: [rf("a.txt", "n1", "e1", "aaa")], local: [lf("a.txt", "aaa")] }).ops)).toEqual(["setBase a.txt"]);
  });

  it("keeps both of two different files met on both sides", () => {
    const op = only(plan({ remote: [rf("a.txt", "n1", "e1", "aaa")], local: [lf("a.txt", "bbb")] }), "conflict");
    expect(op.copyPath).toBe(`a ${TAG}.txt`);
  });

  it("numbers a conflict copy whose name is taken", () => {
    const op = only(plan({ remote: [rf("a.txt", "n1", "e1", "aaa"), rf(`a ${TAG}.txt`, "n2", "e2", "q")], local: [lf("a.txt", "bbb"), lf(`a ${TAG}.txt`, "q")] }), "conflict");
    expect(op.copyPath).toBe(`a ${tag(2)}.txt`);
  });
});

describe("makePlan: shares the user can only read", () => {
  const ro = { writable: false, parentWritable: false };

  it("keeps a local edit of a read-only file as a conflict copy, not uploaded", () => {
    const p = plan({ remote: [rf("s/a.txt", "n1", "e1", "aaa", ro)], local: [ld("s"), lf("s/a.txt", "mine", 2)], base: [bd("s", "s1"), bf("s/a.txt", "n1", "e1", "aaa")], area: { include: ["s"], exclude: [] } });
    expect(only(p, "conflict")).toMatchObject({ upload: false, path: "s/a.txt" });
    expect(p.problems).toEqual([{ kind: "readOnlyEdit", path: `s/a ${TAG}.txt` }]);
  });

  it("doesn't upload a new local file into a read-only folder", () => {
    const p = plan({ remote: [rd("s", "s1", { writable: false, pinned: true, parentWritable: false })], local: [ld("s"), lf("s/n.txt", "n")], base: [bd("s", "s1")], area: { include: ["s"], exclude: [] } });
    expect(p.ops.filter((o) => o.type === "upload")).toEqual([]);
    expect(p.problems).toEqual([{ kind: "readOnlyNew", path: "s/n.txt" }]);
  });

  it("brings back what was deleted locally from a read-only folder", () => {
    const p = plan({ remote: [rd("s", "s1", { writable: false, pinned: true, parentWritable: false }), rf("s/a.txt", "n1", "e1", "aaa", ro)], local: [ld("s")], base: [bd("s", "s1"), bf("s/a.txt", "n1", "e1", "aaa")], area: { include: ["s"], exclude: [] } });
    expect(brief(p.ops)).toEqual(["download s/a.txt"]);
  });

  it("brings back a share deleted locally, never deleting it on the server", () => {
    const p = plan({ remote: [rd("s", "s1", { pinned: true, parentWritable: false }), rf("s/a.txt", "n1", "e1", "aaa")], base: [bd("s", "s1"), bf("s/a.txt", "n1", "e1", "aaa")], area: { include: ["s"], exclude: [] } });
    // Its file is in a folder the user can write to, so deleting it alone
    // would be a delete; with the share gone as a whole, it comes back.
    expect(p.ops.some((o) => o.type === "mkdirLocal" && o.key === "s")).toBe(true);
    expect(p.ops.some((o) => o.type === "trashRemote" && o.key === "s")).toBe(false);
  });
});

describe("makePlan: moves", () => {
  it("moves locally a file the server renamed, without a delete", () => {
    const p = plan({ remote: [rf("new.txt", "n1", "e1", "aaa")], local: [lf("old.txt", "aaa")], base: [bf("old.txt", "n1", "e1", "aaa")] });
    expect(brief(p.ops)).toEqual(["moveLocal new.txt"]);
    expect(only(p, "moveLocal")).toMatchObject({ fromPath: "old.txt", path: "new.txt", nodeId: "n1" });
  });

  it("moves on the server a file renamed locally (same bytes), without an upload or a delete", () => {
    const p = plan({ remote: [rf("old.txt", "n1", "e1", "aaa")], local: [lf("new.txt", "aaa", 7)], base: [bf("old.txt", "n1", "e1", "aaa")] });
    expect(brief(p.ops)).toEqual(["moveRemote new.txt"]);
    expect(only(p, "moveRemote")).toMatchObject({ nodeId: "n1", parentKey: "", name: "new.txt", ifMatch: "e1", fromKey: "old.txt" });
  });

  it("follows a folder the server renamed: files move, the old folder goes without asking", () => {
    const p = plan({
      remote: [rd("B", "d1"), rf("B/x.txt", "n1", "e1", "xxx")],
      local: [ld("A"), lf("A/x.txt", "xxx")],
      base: [bd("A", "d1"), bf("A/x.txt", "n1", "e1", "xxx")],
    });
    expect(brief(p.ops)).toEqual(["mkdirLocal b", "moveLocal b/x.txt", "removeLocal a"]);
    expect(only(p, "removeLocal").confirm).toBe(false);
  });

  it("follows a server move into a folder of another case on disk", () => {
    const p = plan({ remote: [rd("Docs", "d1"), rf("Docs/x.txt", "n1", "e1", "xxx")], local: [ld("docs"), lf("x.txt", "xxx")], base: [bd("docs", "d1"), bf("x.txt", "n1", "e1", "xxx")] });
    expect(only(p, "moveLocal").path).toBe("docs/x.txt");
  });
});

describe("makePlan: folders", () => {
  it("trashes a folder deleted locally after its files", () => {
    const p = plan({ remote: [rd("D", "d1"), rf("D/a", "n1", "e1", "a"), rf("D/b", "n2", "e2", "b")], base: [bd("D", "d1"), bf("D/a", "n1", "e1", "a"), bf("D/b", "n2", "e2", "b")] });
    expect(brief(p.ops)).toEqual(["trashRemote d/a", "trashRemote d/b", "trashRemote d"]);
  });

  it("keeps a folder deleted locally that got a new file on the server", () => {
    const p = plan({ remote: [rd("D", "d1"), rf("D/a", "n1", "e1", "a"), rf("D/new", "n3", "e3", "z")], base: [bd("D", "d1"), bf("D/a", "n1", "e1", "a")] });
    expect(brief(p.ops)).toEqual(["mkdirLocal d", "download d/new", "trashRemote d/a"]);
  });

  it("recreates on the server a folder it deleted while the user added to it", () => {
    const p = plan({ local: [ld("D"), lf("D/old", "o"), lf("D/new", "nn")], base: [bd("D", "d1"), bf("D/old", "n1", "e1", "o")] });
    expect(brief(p.ops)).toEqual(["mkdirRemote d", "upload d/new", "removeLocal d/old"]);
  });

  it("removes a folder the server deleted, its files first, asking", () => {
    const p = plan({ local: [ld("D"), lf("D/a", "a")], base: [bd("D", "d1"), bf("D/a", "n1", "e1", "a")] });
    expect(brief(p.ops)).toEqual(["removeLocal d/a", "removeLocal d"]);
    expect(p.ops.every((o) => o.type === "removeLocal" && o.confirm)).toBe(true);
  });

  it("reports a file on one side and a folder on the other, touching neither", () => {
    const p = plan({ remote: [rd("x", "d1"), rf("x/in.txt", "n1", "e1", "i")], local: [lf("x", "file")] });
    expect(p.ops).toEqual([]);
    expect(p.problems).toEqual([{ kind: "typeMismatch", path: "x" }]);
  });
});

describe("makePlan: names", () => {
  it("matches names case-insensitively, as the server and the disk do", () => {
    const p = plan({ remote: [rf("Report.TXT", "n1", "e1", "aaa")], local: [lf("report.txt", "aaa")] });
    expect(only(p, "setBase").base).toMatchObject({ path: "report.txt", nodeId: "n1" });
  });

  it("downloads into the folder as the disk spells it", () => {
    expect(only(plan({ remote: [rd("Docs", "d1"), rf("Docs/n.txt", "n1", "e1", "n")], local: [ld("docs")] }), "download").path).toBe("docs/n.txt");
  });
});

describe("makePlan: the selected area", () => {
  it("forgets, without deleting, what was unchecked", () => {
    const p = plan({ local: [lf("a.txt", "aaa")], base: [bf("a.txt", "n1", "e1", "aaa")], area: { include: ["docs"], exclude: [] } });
    expect(brief(p.ops)).toEqual(["forget a.txt"]);
  });

  it("makes the folders on the way to a selected one, and syncs nothing else in them", () => {
    const p = plan({
      remote: [rd("A", "a", { container: true }), rd("A/B", "b"), rf("A/B/x", "n1", "e1", "x"), rf("A/other", "n2", "e2", "o")],
      local: [lf("A/mine.txt", "m")],
      area: { include: ["a/b"], exclude: [] },
    });
    expect(brief(p.ops)).toEqual(["mkdirLocal a", "mkdirLocal a/b", "download a/b/x"]);
  });

  it("follows the nearest rule", () => {
    const area = { include: ["", "a/b/c"], exclude: ["a/b"] };
    expect([inArea(area, "x"), inArea(area, "a/b"), inArea(area, "a/b/z"), inArea(area, "a/b/c/d")]).toEqual([true, false, false, true]);
    expect(inArea({ include: [], exclude: [] }, "x")).toBe(false);
  });
});

describe("makePlan: order", () => {
  it("runs folders first, then moves, transfers, then deletes from the bottom", () => {
    const p = plan({
      remote: [rd("gone", "g"), rf("gone/f", "n9", "e9", "f"), rf("old.txt", "n1", "e1", "aaa"), rd("srv", "s")],
      local: [lf("new.txt", "aaa"), ld("loc"), lf("loc/u", "u")],
      base: [bd("gone", "g"), bf("gone/f", "n9", "e9", "f"), bf("old.txt", "n1", "e1", "aaa")],
    });
    expect(brief(p.ops)).toEqual(["mkdirRemote loc", "mkdirLocal srv", "moveRemote new.txt", "upload loc/u", "trashRemote gone/f", "trashRemote gone"]);
  });
});

describe("needsHash", () => {
  it("asks for the content of new files and of those whose stamp moved, only", () => {
    const local = [lf("same", "s"), lf("touched", "t", 9), lf("new", "n"), ld("dir")].map(({ sha256: _, ...l }) => l);
    const base = [bf("same", "1", "e", "s"), bf("touched", "2", "e", "t")];
    expect(needsHash(local, base, ALL).map((l) => l.key)).toEqual(["touched", "new"]);
  });
});
