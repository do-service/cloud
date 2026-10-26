import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PHONE_MEDIA } from "../media";
import { mockBackend, renderApp, openSelectionActions, selectionAction, stubMedia } from "../testing";

const sizes = { offsetHeight: 600, offsetWidth: 800 };
const saved = Object.fromEntries(Object.keys(sizes).map((k) => [k, Object.getOwnPropertyDescriptor(HTMLElement.prototype, k)]));
beforeAll(() => {
  for (const [k, v] of Object.entries(sizes)) Object.defineProperty(HTMLElement.prototype, k, { configurable: true, value: v });
});
afterAll(() => {
  for (const [k, d] of Object.entries(saved)) if (d) Object.defineProperty(HTMLElement.prototype, k, d);
});
afterEach(() => {
  vi.unstubAllGlobals();
});

const alice = () => Response.json({ id: "u1", username: "alice", displayName: "Алиса", isAdmin: false });
const mtime = "2026-10-24T10:00:00Z";
// The owner's rights: everything.
const ALL = ["read", "write", "reshare"];
const node = (id: string, name: string, kind: "dir" | "file" = "file", size = 1) => ({ id, name, kind, size, etag: "e", mtime, permissions: ALL });
const trashed = (id: string, name: string, from: string) => ({ ...node(id, name), trashedAt: "2026-10-24T11:00:00Z", originalPath: from });

describe("TrashPage", () => {
  it("lists the trash and restores, purges and empties it", async () => {
    let items = [trashed("a", "a.txt", "Docs"), trashed("b", "b.txt", ""), trashed("c", "c.txt", "Docs/2026")];
    const calls = mockBackend({
      "GET /api/v1/auth/me": alice,
      "GET /api/v1/trash": () => Response.json({ items, nextCursor: null }),
      "POST /api/v1/trash/a/restore": () => {
        items = items.filter((n) => n.id !== "a");
        return Response.json(node("a", "a.txt"));
      },
      "DELETE /api/v1/trash/b": () => {
        items = items.filter((n) => n.id !== "b");
        return new Response(null, { status: 204 });
      },
      "DELETE /api/v1/trash": () => {
        items = [];
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/trash");
    expect(await screen.findByText("a.txt")).toBeInTheDocument();
    expect(screen.getByText("Docs/2026")).toBeInTheDocument();
    expect(screen.getByText("Мои файлы")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Корзина" })).toHaveAttribute("aria-current", "page");

    await userEvent.click(screen.getByRole("checkbox", { name: "Выбрать «a.txt»" }));
    await userEvent.click(screen.getByRole("button", { name: "Восстановить" }));
    expect(await screen.findByText("Восстановлено: 1")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText("a.txt")).not.toBeInTheDocument());

    // Purging asks first.
    await userEvent.click(screen.getByRole("checkbox", { name: "Выбрать «b.txt»" }));
    await userEvent.click(screen.getByRole("button", { name: "Удалить навсегда" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/Восстановить их будет нельзя/)).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Удалить навсегда" }));
    await waitFor(() => expect(screen.queryByText("b.txt")).not.toBeInTheDocument());
    expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/v1/trash/b")).toBe(true);

    await userEvent.click(screen.getByRole("button", { name: "Очистить корзину" }));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Удалить навсегда" }));
    expect(await screen.findByText("Корзина пуста")).toBeInTheDocument();
  });

  it("puts size and date under the name on a phone", async () => {
    stubMedia(PHONE_MEDIA);
    mockBackend({
      "GET /api/v1/auth/me": alice,
      "GET /api/v1/trash": () => Response.json({ items: [trashed("a", "a.txt", "Docs")], nextCursor: null }),
    });
    renderApp("/trash");
    const table = await screen.findByRole("table");
    expect(within(table).queryByRole("columnheader", { name: "Удалено" })).not.toBeInTheDocument();
    const cell = within(table).getByText("a.txt").closest("td");
    if (!cell) throw new Error("no cell");
    expect(cell).toHaveTextContent(/1 Б · /);

    await userEvent.click(screen.getByRole("button", { name: "Очистить корзину" }));
    expect(await screen.findByRole("dialog")).toHaveAttribute("data-full-screen");
  });
});

describe("VersionsDialog", () => {
  it("shows a file's history and restores a version", async () => {
    const calls = mockBackend({
      "GET /api/v1/auth/me": alice,
      "GET /api/v1/nodes/root": () => Response.json({ ...node("r", "", "dir"), path: [{ id: "r", name: "" }] }),
      "GET /api/v1/nodes/root/children": () => Response.json({ items: [node("d", "Docs", "dir"), node("f", "plan.docx", "file", 2048)], nextCursor: null }),
      "GET /api/v1/nodes/f/versions": () =>
        Response.json({
          items: [
            { id: "v2", size: 1024, createdAt: "2026-10-23T10:00:00Z", author: { id: "u2", displayName: "Иван" } },
            { id: "v1", size: 512, createdAt: "2026-10-22T10:00:00Z", author: null },
          ],
          currentAuthor: { id: "u1", displayName: "Алиса" },
        }),
      "POST /api/v1/versions/v1/restore": () => Response.json(node("f", "plan.docx", "file", 512)),
    });
    renderApp("/files");
    const list = await screen.findByRole("listbox");
    await waitFor(() => expect(within(list).getAllByRole("option")).toHaveLength(2));
    // Folders have no versions.
    fireEvent.click(within(list).getAllByRole("option")[0]);
    await openSelectionActions();
    expect(screen.queryByRole("menuitem", { name: "Версии" })).not.toBeInTheDocument();
    fireEvent.click(within(list).getAllByRole("option")[1]);
    await selectionAction("Версии");

    const dialog = await screen.findByRole("dialog", { name: "Версии «plan.docx»" });
    expect(within(dialog).getByText("Текущая")).toBeInTheDocument();
    expect(await within(dialog).findByText("512 Б")).toBeInTheDocument();
    // Each content is credited to whoever wrote it; an unknown author is left out.
    expect(within(dialog).getByText("· Алиса")).toBeInTheDocument();
    expect(within(dialog).getByText("· Иван")).toBeInTheDocument();
    expect(within(dialog).getAllByText(/^· /)).toHaveLength(2);
    const restore = within(dialog).getAllByRole("button", { name: /^Восстановить версию от/ });
    expect(restore).toHaveLength(2);
    await userEvent.click(restore[1]);
    expect(await screen.findByText("Версия восстановлена, прежняя сохранена в истории")).toBeInTheDocument();
    expect(calls.some((c) => c.method === "POST" && c.url.pathname === "/api/v1/versions/v1/restore")).toBe(true);
  });
});
