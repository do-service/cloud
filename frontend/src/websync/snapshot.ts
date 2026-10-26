import { isIgnored, join, localNameAllowed, nameKey } from "./names";
import type { Area, BaseItem, Problem, RemoteItem } from "./plan";
import type { SyncRoot, TreeNode } from "./remote";
import { SHARED_ID, type Rule } from "./selection";

// The remote side of a snapshot: the trees of the selected roots, laid out
// as on disk — the own root is the chosen folder itself, a share is
// «Доступно мне/<name>» in it (as in WebDAV and the desktop client) — and
// cut down to the selected area and the folders on the way to it.

export type RemoteTree = { root: SyncRoot; nodes: TreeNode[] };

export type RemoteInput = {
  own?: RemoteTree;
  shares: RemoteTree[];
  rules: Rule[];
  // The local name of «Доступно мне».
  sharedName: string;
  // Names Windows can't store aren't downloaded there.
  windows: boolean;
  // To place what a rule names that the server no longer has.
  base: BaseItem[];
};

export type RemoteView = {
  remote: RemoteItem[];
  area: Area;
  problems: Problem[];
  // Keys of the folders on the way to the area: walked by the local scan
  // without being synced themselves.
  descend: Set<string>;
};

export function buildRemote(input: RemoteInput): RemoteView {
  const rules = new Map(input.rules.map((r) => [r.id, r]));
  const remote: RemoteItem[] = [];
  const problems: Problem[] = [];
  const area: Area = { include: [], exclude: [] };
  const seen = new Set<string>();
  const sharedKey = nameKey(input.sharedName);
  // What's in a share's or the own tree goes below this one; none of the
  // own root's own files may take its name.
  area.exclude.push(sharedKey);

  const walkTree = (tree: RemoteTree, top: { path: string; key: string; on: boolean; pinned: boolean; parentWritable: boolean }) => {
    const [root, ...rest] = tree.nodes;
    const kids = new Map<string, TreeNode[]>();
    for (const n of rest) {
      if (n.parentId === null) continue;
      const list = kids.get(n.parentId);
      if (list) list.push(n);
      else kids.set(n.parentId, [n]);
    }
    const visit = (n: TreeNode, path: string, key: string, inherited: boolean, parentWritable: boolean, isTop: boolean): boolean => {
      seen.add(n.id);
      const rule = rules.get(n.id);
      const on = isTop ? top.on : rule ? rule.mode === "include" : inherited;
      if (isTop ? top.on : rule) (on ? area.include : area.exclude).push(key);
      const writable = n.permissions.includes("write");
      let any = on;
      for (const c of n.kind === "dir" ? (kids.get(n.id) ?? []) : []) {
        const cOn = rules.get(c.id) ? rules.get(c.id)?.mode === "include" : on;
        const cPath = join(path, c.name);
        const cKey = join(key, nameKey(c.name));
        if (isIgnored(c.name)) continue;
        if (!localNameAllowed(c.name, input.windows)) {
          if (cOn) problems.push({ kind: "nameNotAllowed", path: cPath });
          continue;
        }
        if (cKey === sharedKey) {
          // An own folder named like «Доступно мне» would mix with the
          // shares on disk.
          if (cOn) problems.push({ kind: "reservedName", path: cPath });
          continue;
        }
        if (visit(c, cPath, cKey, on, writable, false)) any = true;
      }
      if (any) {
        remote.push({
          key,
          path,
          id: n.id,
          kind: n.kind,
          size: n.size,
          etag: n.etag,
          sha256: n.sha256,
          writable,
          parentWritable,
          pinned: isTop && top.pinned,
          container: !on,
          root: tree.root.id,
        });
      }
      return any;
    };
    return visit(root, top.path, top.key, false, top.parentWritable, true);
  };

  if (input.own) {
    const id = input.own.root.id;
    walkTree(input.own, { path: "", key: "", on: rules.get(id)?.mode === "include", pinned: false, parentWritable: false });
  }

  const sharedRule = rules.get(SHARED_ID);
  let anyShare = false;
  for (const tree of input.shares) {
    const name = tree.root.name;
    const rule = rules.get(tree.root.id) ?? sharedRule;
    if (!localNameAllowed(name, input.windows)) {
      problems.push({ kind: "nameNotAllowed", path: join(input.sharedName, name) });
      continue;
    }
    const on = rule?.mode === "include";
    const key = join(sharedKey, nameKey(name));
    if (walkTree(tree, { path: join(input.sharedName, name), key, on, pinned: true, parentWritable: false })) anyShare = true;
  }
  if (anyShare) {
    remote.push({
      key: sharedKey,
      path: input.sharedName,
      id: SHARED_ID,
      kind: "dir",
      size: 0,
      etag: "",
      writable: false,
      parentWritable: false,
      pinned: true,
      container: true,
      root: SHARED_ID,
    });
  }

  // A rule on a node the trees no longer have (deleted on the server, a
  // share taken back): base knows where it was, and what was synced there
  // is still the area — its files are offered for deletion, not dropped
  // from sync unnoticed.
  const baseById = new Map(input.base.map((b) => [b.nodeId, b]));
  for (const r of input.rules) {
    if (r.id === SHARED_ID || seen.has(r.id)) continue;
    const b = baseById.get(r.id);
    if (b) (r.mode === "include" ? area.include : area.exclude).push(b.key);
  }

  const descend = new Set<string>();
  for (const k of area.include) {
    for (let i = k.indexOf("/"); i >= 0; i = k.indexOf("/", i + 1)) descend.add(k.slice(0, i));
  }
  return { remote, area, problems, descend };
}
