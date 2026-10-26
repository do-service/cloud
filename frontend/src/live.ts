import { useQueryClient, type Query, type QueryClient } from "@tanstack/react-query";
import { useEffect, useSyncExternalStore } from "react";
import { ApiError, apiRequest } from "./api";
import { isUser, meQueryKey } from "./auth";
import { ROOT } from "./files";
import { expectShape, isArrayOf, isBoolean, isRecord, isString } from "./guards";

// Live updates: the change journal the desktop client follows
// (/api/v1/sync/changes, docs/desktop-client.md) tells this tab what other
// clients changed, and the queries showing it are invalidated — only the
// ones on screen refetch, the rest are just marked stale.

// A long poll waits this long (the server's most) for a change.
export const LONG_POLL_WAIT = 55;
// Backoff after a failed request: doubles from the first to the last.
export const BACKOFF_MIN = 1000;
export const BACKOFF_MAX = 30_000;
// An empty answer to a waiting request sooner than this is not a timeout
// (the server shutting down, a proxy cutting it): the next one waits a
// little, so such a loop can't spin.
const MIN_EMPTY_INTERVAL = 1000;

// The query families that show nodes; versions and the trash included.
const FILE_QUERIES = new Set(["children", "folders", "node", "search", "trash", "sharedWithMe", "versions"]);

// Mirrors backend/internal/sync's change JSON, with only what is used here.
type ChangedNode = { id: string; parentId: string | null };
type Change = { id: string; deleted: boolean; node?: ChangedNode; subtree?: boolean };
type ChangesPage = { changes: Change[]; cursor: string; more: boolean; rootsChanged: boolean };
type Roots = { roots: { id: string; kind: string }[]; cursor: string };

function isChangedNode(v: unknown): v is ChangedNode {
  return isRecord(v) && isString(v.id) && (v.parentId === null || isString(v.parentId));
}

function isChange(v: unknown): v is Change {
  return (
    isRecord(v) &&
    isString(v.id) &&
    isBoolean(v.deleted) &&
    (v.node === undefined || isChangedNode(v.node)) &&
    (v.subtree === undefined || isBoolean(v.subtree))
  );
}

function isChangesPage(v: unknown): v is ChangesPage {
  return isRecord(v) && isArrayOf(v.changes, isChange) && isString(v.cursor) && isBoolean(v.more) && isBoolean(v.rootsChanged);
}

function isRoots(v: unknown): v is Roots {
  return isRecord(v) && isString(v.cursor) && isArrayOf(v.roots, (r): r is { id: string; kind: string } => isRecord(r) && isString(r.id) && isString(r.kind));
}

function isFileQuery(q: Query): boolean {
  const family = q.queryKey[0];
  return isString(family) && FILE_QUERIES.has(family);
}

// The ids of the nodes a query's data shows: a node (and the folders of
// its path), a list, or the pages of an infinite list.
function shownIds(data: unknown): string[] {
  const ids: string[] = [];
  const add = (v: unknown) => {
    if (isRecord(v) && isString(v.id)) ids.push(v.id);
  };
  const addList = (v: unknown) => {
    if (Array.isArray(v)) v.forEach(add);
  };
  if (Array.isArray(data)) addList(data);
  else if (isRecord(data)) {
    add(data);
    addList(data.path);
    addList(data.items);
    if (Array.isArray(data.pages)) data.pages.forEach((p) => isRecord(p) && addList(p.items));
  }
  return ids;
}

// Invalidates what a page of changes makes stale. A change names the node
// and its folder now, not where it was, so the cache is searched for the
// node too: that finds the folder it left, the trash it came from, a path
// it is on. A listing showing the node's folder refetches as well, for the
// folder's size; sizes further up wait for the next refetch.
export function invalidateChanges(client: QueryClient, page: ChangesPage, ownRoot: string | null): void {
  if (page.rootsChanged) {
    // Rights may have changed anywhere in what is shared: no telling what.
    invalidateFiles(client);
    return;
  }
  if (page.changes.length === 0) return;
  const touched = new Set<string>();
  const folders = new Set<string>();
  let deleted = false;
  let top = false;
  for (const c of page.changes) {
    touched.add(c.id);
    if (c.deleted) deleted = true;
    if (c.subtree) folders.add(c.id);
    if (c.node) {
      if (c.node.parentId === null) top = true;
      else folders.add(c.node.parentId);
    }
  }
  // The own root is listed under its alias as well as its id.
  if (ownRoot !== null && folders.has(ownRoot)) folders.add(ROOT);
  const hit = (id: unknown) => isString(id) && (touched.has(id) || folders.has(id));

  void client.invalidateQueries({
    predicate: (q) => {
      const [family, id] = q.queryKey;
      switch (family) {
        case "children":
        case "folders":
        case "node":
          return hit(id) || shownIds(q.state.data).some(hit);
        case "versions":
          return hit(id);
        case "trash":
          // A deleted node may be in the trash now; a restored one is found
          // in it.
          return deleted || shownIds(q.state.data).some(hit);
        case "sharedWithMe":
          return top || shownIds(q.state.data).some(hit);
        case "search":
          // A new or renamed node may match a search now.
          return true;
        default:
          return false;
      }
    },
  });
}

// Every query of nodes, for when what changed is unknown. A refetch
// already under way goes on: cancelling it would only ask again.
function invalidateFiles(client: QueryClient): void {
  void client.invalidateQueries({ predicate: isFileQuery }, { cancelRefetch: false });
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(done, ms);
    function done() {
      clearTimeout(timer);
      signal.removeEventListener("abort", done);
      resolve();
    }
    signal.addEventListener("abort", done, { once: true });
  });
}

// Resolves once the tab is visible, or on abort.
function whenVisible(signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    function check() {
      if (document.hidden && !signal.aborted) return;
      document.removeEventListener("visibilitychange", check);
      signal.removeEventListener("abort", check);
      resolve();
    }
    document.addEventListener("visibilitychange", check);
    signal.addEventListener("abort", check, { once: true });
    check();
  });
}

// Full jitter over the upper half: tabs that lost the server together
// don't all come back at once.
export function backoff(failures: number, random: () => number = Math.random): number {
  const ceiling = Math.min(BACKOFF_MAX, BACKOFF_MIN * 2 ** Math.max(0, failures - 1));
  return ceiling / 2 + (random() * ceiling) / 2;
}

// Follows the journal until stop() is called: a cursor from /sync/roots,
// then long polls from it. A hidden tab doesn't poll; when it shows again
// everything shown is refetched once and the poll goes on from the same
// cursor. An expired cursor starts over with a new one. 401 and 404 (a
// server without the sync API) stop it.
export function startLiveUpdates(client: QueryClient): () => void {
  const life = new AbortController();
  // The request in flight; aborted when the tab hides or on stop().
  let request: AbortController | null = null;
  const onVisibility = () => {
    if (document.hidden) request?.abort();
  };
  document.addEventListener("visibilitychange", onVisibility);
  life.signal.addEventListener("abort", () => request?.abort(), { once: true });

  void (async () => {
    let cursor: string | null = null;
    let ownRoot: string | null = null;
    let more = false;
    let failures = 0;
    let first = true;
    while (!life.signal.aborted) {
      if (document.hidden) {
        await whenVisible(life.signal);
        if (life.signal.aborted) return;
        invalidateFiles(client);
      }
      request = new AbortController();
      const { signal } = request;
      try {
        if (cursor === null) {
          const roots = expectShape(await apiRequest("/sync/roots", { signal }), isRoots, "GET /sync/roots");
          cursor = roots.cursor;
          ownRoot = roots.roots.find((r) => r.kind === "own")?.id ?? null;
          // Changes between the expired cursor and this one are lost:
          // refetch all instead. The first start has nothing stale.
          if (!first) invalidateFiles(client);
          first = false;
        }
        const wait = more ? 0 : LONG_POLL_WAIT;
        const params = new URLSearchParams({ cursor, wait: String(wait) });
        const started = Date.now();
        const page: ChangesPage = expectShape(await apiRequest(`/sync/changes?${params}`, { signal }), isChangesPage, "GET /sync/changes");
        cursor = page.cursor;
        more = page.more;
        failures = 0;
        invalidateChanges(client, page, ownRoot);
        const empty = page.changes.length === 0 && !page.more && !page.rootsChanged;
        if (empty && wait > 0 && Date.now() - started < MIN_EMPTY_INTERVAL) await sleep(MIN_EMPTY_INTERVAL, life.signal);
      } catch (err) {
        if (life.signal.aborted) return;
        // Hidden mid-request: the loop waits for the tab to show.
        if (signal.aborted) continue;
        if (err instanceof ApiError && err.status === 410) {
          cursor = null;
          continue;
        }
        if (err instanceof ApiError && err.status === 401) {
          // The session ended: re-checking "me" sends the user to sign in,
          // and a new sign-in starts this again (useLiveUpdates).
          void client.invalidateQueries({ queryKey: meQueryKey });
          return;
        }
        if (err instanceof ApiError && err.status === 404) return;
        failures++;
        await sleep(backoff(failures), life.signal);
      } finally {
        request = null;
      }
    }
  })();

  return () => {
    life.abort();
    document.removeEventListener("visibilitychange", onVisibility);
  };
}

// Who is signed in, as the cache knows it; null while signed out or "me"
// failed, so a new sign-in (or a session found again) restarts the
// updates. Not when "me" was read: it is refetched on every window focus,
// and a restart there would drop what changed while it reconnects.
function signedInKey(client: QueryClient): string | null {
  const q = client.getQueryCache().find({ queryKey: meQueryKey, exact: true });
  if (!q || q.state.status !== "success" || !isUser(q.state.data)) return null;
  return q.state.data.id;
}

// Runs live updates while a user is signed in; mounted once per tab, so a
// tab holds one long poll whatever page is open. It only watches "me": the
// pages fetch it.
export function useLiveUpdates(): void {
  const client = useQueryClient();
  const key = useSyncExternalStore(
    (onChange) => client.getQueryCache().subscribe(onChange),
    () => signedInKey(client),
  );
  useEffect(() => {
    if (key === null) return;
    return startLiveUpdates(client);
  }, [client, key]);
}

// The component form of useLiveUpdates, for main.tsx.
export function LiveUpdates(): null {
  useLiveUpdates();
  return null;
}
