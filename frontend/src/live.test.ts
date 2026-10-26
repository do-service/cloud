import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClientProvider, type QueryClient, type QueryKey } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { childrenQueryKey, nodeQueryKey, sharedWithMeQueryKey, trashQueryKey, versionsQueryKey, type Sort } from "./files";
import { BACKOFF_MAX, backoff, LONG_POLL_WAIT, startLiveUpdates, useLiveUpdates } from "./live";
import { createQueryClient } from "./queryClient";

const OWN_ROOT = "r0";
const sort: Sort = { key: "name", order: "asc" };

type Reply = Response | "hang";

// A fake server behind fetch: /sync/roots answers with a cursor, each
// /sync/changes takes the next reply queued in `changes` (or hangs until
// aborted, as a long poll with nothing new does).
function fakeServer() {
  const changes: Reply[] = [];
  const requests: URL[] = [];
  let roots: () => Response = () => Response.json({ roots: [{ id: OWN_ROOT, kind: "own" }], cursor: "c0" });
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://localhost");
      requests.push(url);
      if (url.pathname === "/api/v1/sync/roots") return Promise.resolve(roots());
      const reply = changes.shift() ?? "hang";
      if (reply !== "hang") return Promise.resolve(reply);
      return new Promise<Response>((_, reject) => {
        init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
      });
    }),
  );
  return {
    changes,
    requests,
    setRoots: (f: () => Response) => {
      roots = f;
    },
    paths: () => requests.map((u) => u.pathname.replace("/api/v1/sync/", "")),
  };
}

function page(changes: unknown[], extra: { more?: boolean; rootsChanged?: boolean; cursor?: string } = {}): Response {
  return Response.json({ changes, cursor: extra.cursor ?? "c1", more: extra.more ?? false, rootsChanged: extra.rootsChanged ?? false });
}

const fileNode = (id: string) => ({ id, name: id, kind: "file", size: 1, etag: "e", mtime: "2026-01-01T00:00:00Z", permissions: ["read"] });

function listing(...ids: string[]) {
  return { pages: [{ items: ids.map(fileNode), nextCursor: null }], pageParams: [""] };
}

function setHidden(hidden: boolean) {
  Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
  document.dispatchEvent(new Event("visibilitychange"));
}

function invalidated(client: QueryClient, key: QueryKey): boolean {
  return client.getQueryState(key)?.isInvalidated ?? false;
}

let client: QueryClient;
let stop: (() => void) | undefined;

beforeEach(() => {
  client = createQueryClient(false);
  setHidden(false);
});

afterEach(() => {
  stop?.();
  stop = undefined;
  client.clear();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("live updates", () => {
  it("invalidates the folders a change touches and nothing else", async () => {
    const server = fakeServer();
    client.setQueryData(childrenQueryKey("A", sort), listing("x"));
    client.setQueryData(childrenQueryKey("B", sort), listing());
    client.setQueryData(childrenQueryKey("C", sort), listing("z"));
    // The folder above B shows B, and so its size.
    client.setQueryData(childrenQueryKey("P", sort), listing("B"));
    client.setQueryData(nodeQueryKey("x"), fileNode("x"));
    client.setQueryData(nodeQueryKey("C"), { ...fileNode("C"), path: [] });
    client.setQueryData(versionsQueryKey("x"), { items: [], currentAuthor: null });
    client.setQueryData(trashQueryKey, listing());
    client.setQueryData(sharedWithMeQueryKey, []);
    // x moved from A to B.
    server.changes.push(page([{ id: "x", owner: "u", deleted: false, node: { ...fileNode("x"), parentId: "B" } }]));

    stop = startLiveUpdates(client);

    await vi.waitFor(() => expect(invalidated(client, childrenQueryKey("B", sort))).toBe(true));
    expect(invalidated(client, childrenQueryKey("A", sort))).toBe(true);
    expect(invalidated(client, childrenQueryKey("P", sort))).toBe(true);
    expect(invalidated(client, nodeQueryKey("x"))).toBe(true);
    expect(invalidated(client, versionsQueryKey("x"))).toBe(true);
    expect(invalidated(client, childrenQueryKey("C", sort))).toBe(false);
    expect(invalidated(client, nodeQueryKey("C"))).toBe(false);
    expect(invalidated(client, trashQueryKey)).toBe(false);
    expect(invalidated(client, sharedWithMeQueryKey)).toBe(false);
    expect(server.paths()).toEqual(["roots", "changes", "changes"]);
    expect(server.requests[1].searchParams.get("cursor")).toBe("c0");
    expect(server.requests[1].searchParams.get("wait")).toBe(String(LONG_POLL_WAIT));
    // The next poll goes on from the cursor the last answer gave.
    expect(server.requests[2].searchParams.get("cursor")).toBe("c1");
  });

  it("refetches the trash on a delete", async () => {
    const server = fakeServer();
    client.setQueryData(childrenQueryKey("root", sort), listing("y"));
    client.setQueryData(childrenQueryKey("C", sort), listing("z"));
    client.setQueryData(trashQueryKey, listing());
    server.changes.push(page([{ id: "y", owner: "u", deleted: true }]));

    stop = startLiveUpdates(client);

    await vi.waitFor(() => expect(invalidated(client, trashQueryKey)).toBe(true));
    expect(invalidated(client, childrenQueryKey("root", sort))).toBe(true);
    expect(invalidated(client, childrenQueryKey("C", sort))).toBe(false);
  });

  it("finds the root's listing under its alias", async () => {
    const server = fakeServer();
    client.setQueryData(childrenQueryKey("root", sort), listing("y"));
    client.setQueryData(childrenQueryKey("C", sort), listing("z"));
    server.changes.push(page([{ id: "n", owner: "u", deleted: false, node: { ...fileNode("n"), parentId: OWN_ROOT } }]));

    stop = startLiveUpdates(client);

    await vi.waitFor(() => expect(invalidated(client, childrenQueryKey("root", sort))).toBe(true));
    expect(invalidated(client, childrenQueryKey("C", sort))).toBe(false);
  });

  it("refetches shared lists and everything else when the roots changed", async () => {
    const server = fakeServer();
    client.setQueryData(childrenQueryKey("C", sort), listing("z"));
    client.setQueryData(sharedWithMeQueryKey, []);
    client.setQueryData(["me"], { id: "u" });
    server.changes.push(page([], { rootsChanged: true }));

    stop = startLiveUpdates(client);

    await vi.waitFor(() => expect(invalidated(client, sharedWithMeQueryKey)).toBe(true));
    expect(invalidated(client, childrenQueryKey("C", sort))).toBe(true);
    expect(invalidated(client, ["me"])).toBe(false);
  });

  it("invalidates nothing on an empty answer", async () => {
    const server = fakeServer();
    client.setQueryData(childrenQueryKey("C", sort), listing("z"));
    server.changes.push(page([]));

    stop = startLiveUpdates(client);

    // The empty answer came at once: the next poll waits a second.
    await vi.waitFor(() => expect(server.paths()).toEqual(["roots", "changes", "changes"]), { timeout: 2000 });
    expect(invalidated(client, childrenQueryKey("C", sort))).toBe(false);
  });

  it("reads on without waiting while there is more", async () => {
    const server = fakeServer();
    server.changes.push(page([{ id: "a", owner: "u", deleted: true }], { more: true }), page([]));

    stop = startLiveUpdates(client);

    await vi.waitFor(() => expect(server.requests).toHaveLength(4));
    expect(server.requests.map((u) => u.searchParams.get("wait"))).toEqual([null, String(LONG_POLL_WAIT), "0", String(LONG_POLL_WAIT)]);
  });

  it("starts over from a new cursor when the old one expired", async () => {
    const server = fakeServer();
    client.setQueryData(childrenQueryKey("C", sort), listing("z"));
    server.changes.push(Response.json({ code: "cursor_expired", message: "gone" }, { status: 410 }));

    stop = startLiveUpdates(client);

    await vi.waitFor(() => expect(server.paths()).toEqual(["roots", "changes", "roots", "changes"]));
    // What changed in between is unknown: everything shown refetches.
    expect(invalidated(client, childrenQueryKey("C", sort))).toBe(true);
  });

  it("doesn't poll while the tab is hidden and refetches once it shows", async () => {
    const server = fakeServer();
    client.setQueryData(childrenQueryKey("C", sort), listing("z"));
    setHidden(true);

    stop = startLiveUpdates(client);
    await new Promise((r) => setTimeout(r, 20));
    expect(server.requests).toHaveLength(0);

    setHidden(false);
    await vi.waitFor(() => expect(server.paths()).toEqual(["roots", "changes"]));
    expect(invalidated(client, childrenQueryKey("C", sort))).toBe(true);

    // Hiding drops the waiting request; showing again goes on with the
    // same cursor, without a new one.
    await client.resetQueries({ queryKey: ["children"] });
    client.setQueryData(childrenQueryKey("C", sort), listing("z"));
    setHidden(true);
    await new Promise((r) => setTimeout(r, 20));
    expect(server.requests).toHaveLength(2);
    setHidden(false);
    await vi.waitFor(() => expect(server.paths()).toEqual(["roots", "changes", "changes"]));
    expect(server.requests[2].searchParams.get("cursor")).toBe("c0");
    expect(invalidated(client, childrenQueryKey("C", sort))).toBe(true);
  });

  it("backs off after errors and resets once a request works", async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, "random").mockReturnValue(1);
    const server = fakeServer();
    server.changes.push(new Response("bad gateway", { status: 502 }), new Response("bad gateway", { status: 502 }), page([]));

    stop = startLiveUpdates(client);

    await vi.advanceTimersByTimeAsync(0);
    expect(server.paths()).toEqual(["roots", "changes"]);
    await vi.advanceTimersByTimeAsync(999);
    expect(server.requests).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(server.requests).toHaveLength(3);
    // Twice as long after the second failure.
    await vi.advanceTimersByTimeAsync(1999);
    expect(server.requests).toHaveLength(3);
    await vi.advanceTimersByTimeAsync(1);
    // That one succeeded at once with nothing new, which isn't a timeout:
    // the next poll waits a second rather than spin.
    expect(server.requests).toHaveLength(4);
    await vi.advanceTimersByTimeAsync(1000);
    expect(server.requests).toHaveLength(5);
  });

  it("backs off on network errors too", async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, "random").mockReturnValue(1);
    const fetch = vi.fn(() => Promise.reject(new TypeError("Failed to fetch")));
    vi.stubGlobal("fetch", fetch);

    stop = startLiveUpdates(client);

    await vi.advanceTimersByTimeAsync(0);
    expect(fetch).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1000 + 2000 + 4000);
    expect(fetch).toHaveBeenCalledTimes(4);
  });

  it("caps the backoff with jitter", () => {
    expect(backoff(1, () => 0)).toBe(500);
    expect(backoff(1, () => 1)).toBe(1000);
    expect(backoff(20, () => 1)).toBe(BACKOFF_MAX);
    expect(backoff(20, () => 0)).toBe(BACKOFF_MAX / 2);
  });

  it("stops on a server without the sync API and on a signed-out session", async () => {
    const server = fakeServer();
    server.setRoots(() => Response.json({ code: "not_found", message: "no" }, { status: 404 }));
    stop = startLiveUpdates(client);
    await new Promise((r) => setTimeout(r, 20));
    expect(server.paths()).toEqual(["roots"]);
    stop();

    server.setRoots(() => Response.json({ code: "unauthorized", message: "no" }, { status: 401 }));
    client.setQueryData(["me"], { id: "u" });
    stop = startLiveUpdates(client);
    await vi.waitFor(() => expect(invalidated(client, ["me"])).toBe(true));
    await new Promise((r) => setTimeout(r, 20));
    expect(server.paths()).toEqual(["roots", "roots"]);
  });

  it("aborts the waiting request on stop", async () => {
    const server = fakeServer();
    stop = startLiveUpdates(client);
    await vi.waitFor(() => expect(server.paths()).toEqual(["roots", "changes"]));
    const fetch = vi.mocked(globalThis.fetch);
    const signal = fetch.mock.calls[1][1]?.signal;
    expect(signal?.aborted).toBe(false);
    stop();
    expect(signal?.aborted).toBe(true);
    await new Promise((r) => setTimeout(r, 20));
    expect(server.requests).toHaveLength(2);
  });

  it("runs only while someone is signed in", async () => {
    const server = fakeServer();
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client }, children);
    renderHook(() => useLiveUpdates(), { wrapper });
    await new Promise((r) => setTimeout(r, 20));
    expect(server.requests).toHaveLength(0);

    act(() => client.setQueryData(["me"], { id: "u", username: "u", displayName: "", isAdmin: false }));
    await vi.waitFor(() => expect(server.paths()).toEqual(["roots", "changes"]));
    const signal = vi.mocked(globalThis.fetch).mock.calls[1][1]?.signal;

    // Signing out clears the cache.
    act(() => client.clear());
    await vi.waitFor(() => expect(signal?.aborted).toBe(true));
  });
});
