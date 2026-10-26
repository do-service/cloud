import { MantineProvider } from "@mantine/core";
import { QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { t } from "../i18n";
import { createQueryClient } from "../queryClient";
import { mockBackend } from "../testing";
import { theme } from "../theme";
import { contents, fakeFolder, type FakeDir } from "../websync/fakefs";
import UserMenu from "./UserMenu";
import WebSyncDialog from "./WebSyncDialog";

const me = (id: string) => () => Response.json({ id, username: id, displayName: "", isAdmin: false });

function wrap(ui: React.ReactNode) {
  return render(
    <QueryClientProvider client={createQueryClient(false)}>
      <MantineProvider theme={theme}>
        <MemoryRouter>{ui}</MemoryRouter>
      </MantineProvider>
    </QueryClientProvider>,
  );
}

function stubPicker(folder: FakeDir) {
  Object.defineProperty(window, "showDirectoryPicker", { configurable: true, value: async () => folder });
}

async function sha(s: string): Promise<string> {
  const sum = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s)));
  return Array.from(sum, (b) => b.toString(16).padStart(2, "0")).join("");
}

const node = (id: string, parentId: string | null, name: string, kind: "dir" | "file", etag: string, content = "") =>
  sha(content).then((s) => ({ id, parentId, name, kind, size: content.length, etag, sha256: kind === "file" ? s : undefined, mtime: "2026-10-26T10:00:00Z", permissions: ["read", "write"] }));

async function tree(nodes: Promise<unknown>[]): Promise<Response> {
  const lines = [JSON.stringify({ cursor: "c" }), ...(await Promise.all(nodes)).map((n) => JSON.stringify(n))];
  return new Response(`${lines.join("\n")}\n`, { headers: { "Content-Type": "application/x-ndjson" } });
}

const roots = () => Response.json({ roots: [{ id: "root", kind: "own", name: "", owner: { id: "u", name: "U" }, permissions: ["read", "write", "reshare"], etag: "e" }], cursor: "c" });

afterEach(() => {
  vi.unstubAllGlobals();
  Reflect.deleteProperty(window, "showDirectoryPicker");
});

describe("WebSyncDialog", () => {
  it("is offered disabled, with the reason, where the browser can't do it", async () => {
    mockBackend({ "GET /api/v1/auth/me": me("u0") });
    wrap(<UserMenu />);
    await userEvent.click(await screen.findByRole("button", { name: t("user.menu") }));
    const item = await screen.findByRole("menuitem", { name: new RegExp(t("webSync.menu")) });
    expect(item).toBeDisabled();
    expect(within(item).getByText(t("webSync.unsupported"))).toBeInTheDocument();
  });

  it("syncs a ticked folder into the chosen one", async () => {
    const folder = fakeFolder({});
    stubPicker(folder);
    mockBackend({
      "GET /api/v1/auth/me": me("u1"),
      "GET /api/v1/sync/roots": roots,
      "GET /api/v1/sync/tree": () => tree([node("root", null, "", "dir", "e0"), node("d1", "root", "Docs", "dir", "e1"), node("f1", "d1", "a.txt", "file", "e2", "alpha")]),
      "GET /api/v1/nodes/f1/content": () => new Response("alpha"),
    });
    wrap(<WebSyncDialog opened onClose={() => {}} />);
    const sync = await screen.findByRole("button", { name: t("webSync.sync") });
    expect(sync).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: t("webSync.chooseFolder") }));
    await userEvent.click(await screen.findByRole("checkbox", { name: t("webSync.myFiles") }));
    await waitFor(() => expect(sync).toBeEnabled());
    await userEvent.click(sync);
    expect(await screen.findByText(t("webSync.result.downloaded", { count: 1 }))).toBeInTheDocument();
    expect(contents(folder)).toEqual({ "Docs/a.txt": "alpha" });
  });

  it("lists the deletes and leaves them undone without consent", async () => {
    const folder = fakeFolder({});
    stubPicker(folder);
    let gone = false;
    const deletes: string[] = [];
    mockBackend({
      "GET /api/v1/auth/me": me("u2"),
      "GET /api/v1/sync/roots": roots,
      "GET /api/v1/sync/tree": () => tree([node("root", null, "", "dir", "e0"), ...(gone ? [] : [node("f1", "root", "old.txt", "file", "e2", "old")])]),
      "GET /api/v1/nodes/f1/content": () => new Response("old"),
      "DELETE /api/v1/nodes/f1": () => {
        deletes.push("f1");
        return new Response(null, { status: 204 });
      },
    });
    wrap(<WebSyncDialog opened onClose={() => {}} />);
    await userEvent.click(await screen.findByRole("button", { name: t("webSync.chooseFolder") }));
    await userEvent.click(await screen.findByRole("checkbox", { name: t("webSync.myFiles") }));
    await userEvent.click(screen.getByRole("button", { name: t("webSync.sync") }));
    await screen.findByText(t("webSync.result.downloaded", { count: 1 }));

    gone = true;
    await userEvent.click(screen.getByRole("button", { name: t("webSync.sync") }));
    expect(await screen.findByText(t("webSync.deletes.local", { count: 1 }))).toBeInTheDocument();
    expect(screen.getByText("old.txt")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: t("webSync.deletes.skip") }));
    expect(await screen.findByText(t("webSync.result.deferred", { count: 1 }))).toBeInTheDocument();
    expect(contents(folder)).toEqual({ "old.txt": "old" });

    await userEvent.click(screen.getByRole("button", { name: t("webSync.sync") }));
    await userEvent.click(await screen.findByRole("button", { name: t("webSync.deletes.apply") }));
    expect(await screen.findByText(t("webSync.result.deletedLocal", { count: 1 }))).toBeInTheDocument();
    expect(contents(folder)).toEqual({});
    expect(deletes).toEqual([]);
  });
});
