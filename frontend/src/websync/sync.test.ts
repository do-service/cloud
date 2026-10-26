import { afterEach, describe, expect, it, vi } from "vitest";
import type { FileReader as TusFileReader, HttpRequest, HttpResponse, HttpStack } from "tus-js-client";
import { mockBackend } from "../testing";
import { contents, fakeFolder } from "./fakefs";
import { upload } from "./remote";
import { SHARED_ID, type Rule } from "./selection";
import { MemoryStore } from "./store";
import type { BaseItem, Op } from "./plan";
import { acquire, FolderLooksEmpty, prepare, run, serverApi, summarize, type PassContext } from "./sync";

// Whole passes against a fake server behind fetch (testing.tsx's
// mockBackend) and a fake folder: what lands on each side, what base
// records, and what a 412 or a declined delete leaves.

type Node = {
  id: string;
  parentId: string | null;
  name: string;
  kind: "dir" | "file";
  data: Uint8Array<ArrayBuffer>;
  etag: string;
  permissions: ("read" | "write" | "reshare")[];
};

const enc = (s: string) => new TextEncoder().encode(s);

async function sha(data: Uint8Array<ArrayBuffer>): Promise<string> {
  return Array.from(new Uint8Array(await crypto.subtle.digest("SHA-256", data)), (b) => b.toString(16).padStart(2, "0")).join("");
}

type Handler = (url: URL, init: RequestInit | undefined) => Response | Promise<Response>;

function headerOf(init: RequestInit | undefined, name: string): string | undefined {
  const h = init?.headers;
  if (!h || h instanceof Headers || Array.isArray(h)) return undefined;
  const found = Object.entries(h).find(([k]) => k.toLowerCase() === name.toLowerCase());
  return found?.[1];
}

const err = (status: number, code: string) => Response.json({ code, message: code }, { status });

// A server with one user's tree, the sync API and tus, as far as a pass
// uses them.
class FakeServer {
  nodes = new Map<string, Node>();
  trashed: string[] = [];
  private seq = 0;
  private uploads = new Map<string, { meta: Record<string, string>; size: number; data: Uint8Array<ArrayBuffer> }>();

  constructor() {
    this.nodes.set("root", { id: "root", parentId: null, name: "", kind: "dir", data: new Uint8Array(0), etag: "e0", permissions: ["read", "write", "reshare"] });
  }

  private etag(): string {
    return `e${++this.seq}`;
  }

  add(path: string, content?: string, permissions: Node["permissions"] = ["read", "write", "reshare"]): Node {
    let parent = "root";
    const parts = path.split("/");
    for (const [i, name] of parts.entries()) {
      const last = i === parts.length - 1;
      const existing = [...this.nodes.values()].find((n) => n.parentId === parent && n.name === name);
      if (existing && !last) {
        parent = existing.id;
        continue;
      }
      const kind = last && content !== undefined ? "file" : "dir";
      const n: Node = { id: `n${++this.seq}`, parentId: parent, name, kind, data: enc(content ?? ""), etag: this.etag(), permissions };
      if (existing) this.nodes.delete(existing.id);
      this.nodes.set(n.id, n);
      parent = n.id;
    }
    const node = this.nodes.get(parent);
    if (!node) throw new Error("not added");
    return node;
  }

  find(path: string): Node | undefined {
    let cur: Node | undefined = this.nodes.get("root");
    for (const name of path.split("/")) cur = [...this.nodes.values()].find((n) => n.parentId === cur?.id && n.name === name);
    return cur;
  }

  edit(path: string, content: string) {
    const n = this.find(path);
    if (!n) throw new Error(`no ${path}`);
    n.data = enc(content);
    n.etag = this.etag();
  }

  remove(id: string) {
    for (const c of [...this.nodes.values()].filter((n) => n.parentId === id)) this.remove(c.id);
    this.nodes.delete(id);
  }

  // The tree as paths → content, folders as "path/".
  tree(id = "root", prefix = ""): Record<string, string> {
    const out: Record<string, string> = {};
    for (const n of [...this.nodes.values()].filter((x) => x.parentId === id).sort((a, b) => (a.name < b.name ? -1 : 1))) {
      if (n.kind === "file") out[prefix + n.name] = new TextDecoder().decode(n.data);
      else {
        const sub = this.tree(n.id, `${prefix}${n.name}/`);
        Object.assign(out, Object.keys(sub).length ? sub : { [`${prefix}${n.name}/`]: "" });
      }
    }
    return out;
  }

  private async json(n: Node) {
    return { id: n.id, parentId: n.parentId, name: n.name, kind: n.kind, size: n.data.length, etag: n.etag, sha256: n.kind === "file" ? await sha(n.data) : undefined, mtime: "2026-10-26T10:00:00Z", permissions: n.permissions };
  }

  private async treeLines(rootId: string): Promise<string> {
    const lines = [JSON.stringify({ cursor: "c" })];
    const queue = [rootId];
    while (queue.length) {
      const id = queue.shift();
      const n = id ? this.nodes.get(id) : undefined;
      if (!n) continue;
      lines.push(JSON.stringify(await this.json(n)));
      for (const c of this.nodes.values()) if (c.parentId === n.id) queue.push(c.id);
    }
    return `${lines.join("\n")}\n`;
  }

  private finish(u: { meta: Record<string, string>; data: Uint8Array<ArrayBuffer> }): Node | Response {
    if (u.meta.nodeId) {
      const n = this.nodes.get(u.meta.nodeId);
      if (!n) return err(404, "not_found");
      if (u.meta.ifMatch && u.meta.ifMatch !== n.etag) return err(412, "precondition_failed");
      n.data = u.data;
      n.etag = this.etag();
      return n;
    }
    const parent = this.nodes.get(u.meta.parentId);
    if (!parent) return err(404, "not_found");
    if (!parent.permissions.includes("write")) return err(403, "forbidden");
    if ([...this.nodes.values()].some((c) => c.parentId === parent.id && c.name.toLowerCase() === u.meta.filename.toLowerCase())) return err(409, "name_conflict");
    const n: Node = { id: `n${++this.seq}`, parentId: parent.id, name: u.meta.filename, kind: "file", data: u.data, etag: this.etag(), permissions: parent.permissions };
    this.nodes.set(n.id, n);
    return n;
  }

  route(method: string, path: string): Handler | undefined {
    if (method === "GET" && path === "/api/v1/sync/roots") {
      return () => Response.json({ roots: [{ id: "root", kind: "own", name: "", owner: { id: "u", name: "U" }, permissions: ["read", "write", "reshare"], etag: "e" }, ...this.shares], cursor: "c" });
    }
    if (method === "GET" && path === "/api/v1/sync/tree") {
      return async (url) => new Response(await this.treeLines(url.searchParams.get("root") ?? ""), { headers: { "Content-Type": "application/x-ndjson" } });
    }
    let m = /^\/api\/v1\/nodes\/([^/]+)(\/content|\/children)?$/.exec(path);
    if (m) {
      const id = m[1];
      const sub = m[2];
      const n = this.nodes.get(id);
      if (!n) return () => err(404, "not_found");
      if (method === "GET" && sub === "/content") return () => new Response(n.data);
      if (method === "GET" && sub === "/children") return () => Response.json({ items: [...this.nodes.values()].filter((c) => c.parentId === id).map((c) => ({ id: c.id })), nextCursor: null });
      if (method === "POST" && sub === "/children") {
        return (_, init) => {
          const { name } = JSON.parse(String(init?.body));
          if ([...this.nodes.values()].some((c) => c.parentId === id && c.name.toLowerCase() === String(name).toLowerCase())) return err(409, "name_conflict");
          const d: Node = { id: `n${++this.seq}`, parentId: id, name: String(name), kind: "dir", data: new Uint8Array(0), etag: this.etag(), permissions: n.permissions };
          this.nodes.set(d.id, d);
          return Response.json({ ...d, data: undefined, size: 0, mtime: "2026-10-26T10:00:00Z" });
        };
      }
      const ifMatch = (init: RequestInit | undefined) => headerOf(init, "If-Match")?.replace(/"/g, "");
      if (method === "DELETE" && !sub) {
        return (_, init) => {
          const want = ifMatch(init);
          if (want && want !== n.etag) return err(412, "precondition_failed");
          this.remove(id);
          this.trashed.push(n.name);
          return new Response(null, { status: 204 });
        };
      }
      if (method === "PATCH" && !sub) {
        return (_, init) => {
          const want = ifMatch(init);
          if (want && want !== n.etag) return err(412, "precondition_failed");
          const body = JSON.parse(String(init?.body));
          n.parentId = body.parentId ?? n.parentId;
          n.name = body.name ?? n.name;
          return Response.json({ id: n.id, etag: n.etag });
        };
      }
    }
    if (method === "POST" && path === "/api/v1/uploads/") {
      return (_, init) => {
        const meta: Record<string, string> = {};
        for (const pair of (headerOf(init, "Upload-Metadata") ?? "").split(",")) {
          const [k, v] = pair.split(" ");
          meta[k] = v ? new TextDecoder().decode(Uint8Array.from(atob(v), (c) => c.charCodeAt(0))) : "";
        }
        if (meta.nodeId && meta.ifMatch && this.nodes.get(meta.nodeId)?.etag !== meta.ifMatch) return err(412, "precondition_failed");
        const size = Number(headerOf(init, "Upload-Length"));
        const id = `u${++this.seq}`;
        this.uploads.set(id, { meta, size, data: new Uint8Array(0) });
        if (size === 0) {
          const done = this.finish({ meta, data: new Uint8Array(0) });
          if (done instanceof Response) return done;
          return new Response(null, { status: 201, headers: { Location: `http://localhost/api/v1/uploads/${id}`, "Upload-Node-Id": done.id, "Upload-Node-ETag": done.etag } });
        }
        return new Response(null, { status: 201, headers: { Location: `http://localhost/api/v1/uploads/${id}` } });
      };
    }
    m = /^\/api\/v1\/uploads\/(u\d+)$/.exec(path);
    if (method === "PATCH" && m) {
      const u = this.uploads.get(m[1]);
      return (_, init) => {
        if (!u || !(init?.body instanceof Uint8Array)) return err(404, "not_found");
        const all = new Uint8Array(u.data.length + init.body.length);
        all.set(u.data);
        all.set(init.body, u.data.length);
        u.data = all;
        if (u.data.length < u.size) return new Response(null, { status: 204, headers: { "Upload-Offset": String(u.data.length) } });
        const done = this.finish(u);
        if (done instanceof Response) return done;
        return new Response(null, { status: 204, headers: { "Upload-Offset": String(u.data.length), "Upload-Node-Id": done.id, "Upload-Node-ETag": done.etag } });
      };
    }
    return undefined;
  }

  shares: unknown[] = [];

  // Routes for mockBackend, looked up as it asks for them.
  routes(): Record<string, Handler> {
    return new Proxy<Record<string, Handler>>(
      {},
      {
        get: (_, key) => {
          if (typeof key !== "string") return undefined;
          const [method, path] = key.split(" ");
          return this.route(method, path);
        },
      },
    );
  }
}

// tus-js-client over fetch, so its requests reach mockBackend.
const fetchStack: HttpStack = {
  getName: () => "fetch",
  createRequest(method: string, url: string): HttpRequest {
    const headers: Record<string, string> = {};
    return {
      getMethod: () => method,
      getURL: () => url,
      setHeader: (h, v) => {
        headers[h] = v;
      },
      getHeader: (h) => headers[h],
      setProgressHandler: () => {},
      send: async (body: unknown): Promise<HttpResponse> => {
        const data = body instanceof Blob ? new Uint8Array(await body.arrayBuffer()) : undefined;
        const res = await fetch(url, { method, headers, body: data });
        const text = await res.text();
        return { getStatus: () => res.status, getHeader: (h) => res.headers.get(h) ?? undefined, getBody: () => text, getUnderlyingObject: () => res };
      },
      abort: async () => {},
      getUnderlyingObject: () => null,
    };
  },
};

const blobReader: TusFileReader = {
  async openFile(input: unknown) {
    if (!(input instanceof Blob)) throw new Error("not a blob");
    return {
      size: input.size,
      // tus-js-client sizes a slice by its .size: a Blob's.
      slice: async (start: number, end: number) => ({ value: input.slice(start, end), done: end >= input.size }),
      close: () => {},
    };
  },
};

const ALL: Rule[] = [{ id: "root", mode: "include", root: "root", chain: [] }];

function setup(local: Record<string, string> = {}, rules: Rule[] = ALL) {
  const server = new FakeServer();
  const calls = mockBackend(server.routes());
  const root = fakeFolder(local);
  const store = new MemoryStore();
  const ctx: PassContext = {
    store,
    root,
    folderId: "f1",
    rules,
    sharedName: "Shared",
    conflictTag: () => "(conflict, Browser, 2026-10-26 10-00)",
    windows: false,
    api: { ...serverApi, upload: (file, target, options) => upload(file, target, { ...options, tus: { httpStack: fetchStack, fileReader: blobReader } }) },
  };
  return { server, calls, root, store, ctx };
}

async function pass(ctx: PassContext, deletes = false) {
  const p = await prepare(ctx);
  const outcome = await run(ctx, p, deletes);
  return { prepared: p, outcome };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("a pass", () => {
  it("brings the cloud down into an empty folder, then has nothing to do", async () => {
    const { server, root, ctx, store } = setup();
    server.add("docs/a.txt", "alpha");
    server.add("docs/sub/b.txt", "beta");
    server.add("empty");
    const { outcome } = await pass(ctx);
    expect(outcome.problems).toEqual([]);
    expect(outcome.downloaded).toBe(2);
    expect(contents(root)).toEqual({ "docs/a.txt": "alpha", "docs/sub/b.txt": "beta", "empty/": "" });
    expect((await store.base("f1")).map((b) => b.key).sort()).toEqual(["docs", "docs/a.txt", "docs/sub", "docs/sub/b.txt", "empty"]);
    const again = await prepare(ctx);
    expect(again.plan.ops).toEqual([]);
  });

  it("uploads new and changed files, the change conditional on base's ETag", async () => {
    const { server, root, ctx, calls } = setup();
    server.add("a.txt", "one");
    await pass(ctx);
    const before = server.find("a.txt")?.etag;
    await root.addFile("a.txt", enc("one, edited"));
    root.addFile("new.txt", enc("fresh"));
    const { outcome } = await pass(ctx);
    expect(outcome.problems).toEqual([]);
    expect(outcome.uploaded).toBe(2);
    expect(server.tree()).toEqual({ "a.txt": "one, edited", "new.txt": "fresh" });
    const created = calls.filter((c) => c.method === "POST" && c.url.pathname === "/api/v1/uploads/");
    expect(created).toHaveLength(2);
    expect((await prepare(ctx)).plan.ops).toEqual([]);
    expect(before).not.toBe(server.find("a.txt")?.etag);
  });

  it("reports a 412 and leaves base alone, so the next pass sees the conflict", async () => {
    const { server, root, ctx } = setup();
    server.add("a.txt", "one");
    await pass(ctx);
    root.addFile("a.txt", enc("mine"));
    const prepared = await prepare(ctx);
    // Someone else saves between the plan and the upload.
    server.edit("a.txt", "theirs");
    const outcome = await run(ctx, prepared, false);
    expect(outcome.problems).toEqual([{ kind: "precondition", path: "a.txt", code: "precondition_failed" }]);
    expect(server.tree()).toEqual({ "a.txt": "theirs" });
    const next = await pass(ctx);
    expect(next.outcome.conflicts).toBe(1);
    const copy = "a (conflict, Browser, 2026-10-26 10-00).txt";
    expect(contents(root)).toEqual({ "a.txt": "theirs", [copy]: "mine" });
    expect(server.tree()).toEqual({ "a.txt": "theirs", [copy]: "mine" });
  });

  it("deletes only once the user agrees, and offers the deletes again until then", async () => {
    const { server, root, ctx } = setup();
    server.add("keep.txt", "k");
    server.add("gone-local.txt", "l");
    server.add("gone-remote.txt", "r");
    await pass(ctx);
    await root.removeEntry("gone-local.txt");
    const n = server.find("gone-remote.txt");
    if (n) server.remove(n.id);

    const first = await pass(ctx, false);
    expect(first.prepared.summary.deleteRemote).toEqual([{ path: "gone-local.txt", kind: "file" }]);
    expect(first.prepared.summary.deleteLocal).toEqual([{ path: "gone-remote.txt", kind: "file" }]);
    expect(first.outcome.deferred).toBe(2);
    expect(Object.keys(server.tree())).toContain("gone-local.txt");
    expect(Object.keys(contents(root))).toContain("gone-remote.txt");

    const second = await pass(ctx, true);
    expect(second.prepared.summary.deleteRemote).toHaveLength(1);
    expect(second.outcome).toMatchObject({ deletedRemote: 1, deletedLocal: 1, problems: [] });
    expect(server.tree()).toEqual({ "keep.txt": "k" });
    expect(server.trashed).toEqual(["gone-local.txt"]);
    expect(contents(root)).toEqual({ "keep.txt": "k" });
    expect((await prepare(ctx)).plan.ops).toEqual([]);
  });

  it("doesn't delete a file in the cloud that changed after the plan (If-Match)", async () => {
    const { server, root, ctx } = setup();
    server.add("a.txt", "one");
    await pass(ctx);
    await root.removeEntry("a.txt");
    const prepared = await prepare(ctx);
    server.edit("a.txt", "edited meanwhile");
    const outcome = await run(ctx, prepared, true);
    expect(outcome.problems).toEqual([{ kind: "precondition", path: "a.txt", code: "precondition_failed" }]);
    expect(server.tree()).toEqual({ "a.txt": "edited meanwhile" });
    // The edit beats the delete: it comes back.
    await pass(ctx);
    expect(contents(root)).toEqual({ "a.txt": "edited meanwhile" });
  });

  it("never overwrites a local file changed after the plan", async () => {
    const { server, root, ctx } = setup();
    server.add("a.txt", "one");
    await pass(ctx);
    server.edit("a.txt", "server edit");
    const prepared = await prepare(ctx);
    root.addFile("a.txt", enc("typed just now"));
    const outcome = await run(ctx, prepared, false);
    expect(outcome.problems.map((p) => p.kind)).toEqual(["changedMeanwhile"]);
    expect(contents(root)).toEqual({ "a.txt": "typed just now" });
  });

  it("refuses a folder that looks empty when much was synced into it", async () => {
    const { server, ctx } = setup();
    for (const n of ["a", "b", "c"]) server.add(`${n}.txt`, n);
    await pass(ctx);
    const other = { ...ctx, root: fakeFolder({}) };
    await expect(prepare(other)).rejects.toBeInstanceOf(FolderLooksEmpty);
  });

  it("follows renames on both sides without deleting anything", async () => {
    const { server, root, ctx } = setup({}, ALL);
    server.add("old-remote.txt", "r");
    server.add("old-local.txt", "l");
    await pass(ctx);
    const r = server.find("old-remote.txt");
    if (r) r.name = "new-remote.txt";
    const l = root.children.get("old-local.txt");
    if (l && "data" in l) {
      root.children.delete("old-local.txt");
      const moved = root.addFile("new-local.txt", l.data);
      moved.lastModified = l.lastModified;
    }
    const { prepared, outcome } = await pass(ctx);
    expect(prepared.summary.deleteRemote).toEqual([]);
    expect(prepared.summary.deleteLocal).toEqual([]);
    expect(outcome.problems).toEqual([]);
    expect(server.tree()).toEqual({ "new-local.txt": "l", "new-remote.txt": "r" });
    expect(contents(root)).toEqual({ "new-local.txt": "l", "new-remote.txt": "r" });
  });

  it("keeps a local edit of a read-only share as a conflict copy, uploading nothing", async () => {
    const rules: Rule[] = [{ id: SHARED_ID, mode: "include", root: SHARED_ID, chain: [] }];
    const { server, root, ctx } = setup({}, rules);
    const share = server.add("their/s", undefined, ["read"]);
    server.add("their/s/doc.txt", "theirs", ["read"]);
    server.shares = [{ id: share.id, kind: "shared", name: "S (Owner)", owner: { id: "o", name: "Owner" }, permissions: ["read"], etag: "e" }];
    await pass(ctx);
    expect(contents(root)).toEqual({ "Shared/S (Owner)/doc.txt": "theirs" });
    const dir = await (await root.getDirectoryHandle("Shared")).getDirectoryHandle("S (Owner)");
    dir.addFile("doc.txt", enc("my edit"));
    const { outcome } = await pass(ctx);
    expect(outcome.problems.map((p) => p.kind)).toEqual(["readOnlyEdit"]);
    expect(contents(root)).toEqual({ "Shared/S (Owner)/doc (conflict, Browser, 2026-10-26 10-00).txt": "my edit", "Shared/S (Owner)/doc.txt": "theirs" });
    expect(server.tree(share.id)).toEqual({ "doc.txt": "theirs" });
  });

  it("removes nothing half-written when a download's content doesn't match the tree", async () => {
    const { server, root, ctx } = setup();
    server.add("a.txt", "one");
    const prepared = await prepare(ctx);
    server.edit("a.txt", "changed");
    const outcome = await run(ctx, prepared, false);
    expect(outcome.problems.map((p) => p.kind)).toEqual(["changedMeanwhile"]);
    expect(contents(root)).toEqual({});
  });
});

describe("acquire", () => {
  it("lets one pass run at a time", async () => {
    const release = await acquire();
    expect(release).not.toBeNull();
    expect(await acquire()).toBeNull();
    release?.();
    const again = await acquire();
    expect(again).not.toBeNull();
    again?.();
  });
});

describe("summarize", () => {
  const base: BaseItem[] = Array.from({ length: 20 }, (_, i) => ({ key: `f${i}`, path: `f${i}`, nodeId: `n${i}`, kind: "file", etag: "e", size: 1, lastModified: 1 }));
  const trash = (n: number): Op[] => Array.from({ length: n }, (_, i) => ({ type: "trashRemote", key: `f${i}`, path: `f${i}`, nodeId: `n${i}`, kind: "file", confirm: true }));

  it("warns when a large share of what was synced would go", () => {
    expect(summarize(trash(10), base, () => true).mass).toBe(true);
    expect(summarize(trash(5), base, () => true).mass).toBe(false);
    // A handful is never "mass", whatever its share.
    expect(summarize(trash(2), base.slice(0, 2), () => true).mass).toBe(false);
  });
});
