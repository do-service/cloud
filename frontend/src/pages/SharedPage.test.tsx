import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { mockBackend, renderApp, openSelectionActions, selectionAction } from "../testing";

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

const me = () => Response.json({ id: "u1", username: "alice", displayName: "Алиса", isAdmin: false });
const mtime = "2026-10-24T10:00:00Z";
const ALL = ["read", "write", "reshare"];
const node = (id: string, name: string, kind: "dir" | "file", permissions: string[]) => ({ id, name, kind, size: 1, etag: "e", mtime, permissions });

describe("ShareDialog", () => {
  it("shares with someone found in the directory, changes their level and revokes", async () => {
    let shares: { id: string; grantee: { type: string; id: string; name: string }; permissions: string[]; createdAt: string }[] = [];
    const calls = mockBackend({
      "GET /api/v1/auth/me": me,
      "GET /api/v1/nodes/root": () => Response.json({ ...node("r", "", "dir", ALL), path: [{ id: "r", name: "" }] }),
      "GET /api/v1/nodes/root/children": () => Response.json({ items: [node("d", "Docs", "dir", ALL)], nextCursor: null }),
      "GET /api/v1/directory": () => Response.json({ items: [{ type: "user", id: "u2", name: "Боб" }, { type: "group", id: "g1", name: "Бухгалтерия" }] }),
      "GET /api/v1/nodes/d/shares": () => Response.json({ items: shares }),
      "POST /api/v1/nodes/d/shares": () => {
        shares = [{ id: "s1", grantee: { type: "user", id: "u2", name: "Боб" }, permissions: ["read"], createdAt: mtime }];
        return Response.json(shares[0]);
      },
      "PATCH /api/v1/shares/s1": () => {
        shares = [{ ...shares[0], permissions: ["read", "write"] }];
        return Response.json(shares[0]);
      },
      "DELETE /api/v1/shares/s1": () => {
        shares = [];
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/files");
    const list = await screen.findByRole("listbox");
    await waitFor(() => expect(within(list).getAllByRole("option")).toHaveLength(1));
    fireEvent.click(within(list).getAllByRole("option")[0]);
    await selectionAction("Поделиться");

    const dialog = await screen.findByRole("dialog", { name: "Доступ к «Docs»" });
    expect(await within(dialog).findByText("Пока ни у кого, кроме вас.")).toBeInTheDocument();
    await userEvent.type(within(dialog).getByLabelText("Поделиться с"), "Бо");
    // The directory is asked what was typed, after a short pause.
    await waitFor(() => expect(calls.some((c) => c.url.pathname === "/api/v1/directory" && c.url.searchParams.get("q") === "Бо")).toBe(true));
    await userEvent.click(await screen.findByRole("option", { name: "Боб", hidden: true }));
    expect(screen.getByRole("option", { name: "Бухгалтерия (группа)", hidden: true })).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Открыть доступ" }));

    expect(await within(dialog).findByText("Боб")).toBeInTheDocument();
    const post = calls.find((c) => c.method === "POST" && c.url.pathname === "/api/v1/nodes/d/shares");
    expect(post?.body).toEqual({ granteeType: "user", granteeId: "u2", permissions: ["read"] });

    const level = within(dialog).getByLabelText("Доступ для Боб");
    await userEvent.click(level);
    // Each select has its own options list; this row's is the one it controls.
    const options = await waitFor(() => {
      const el = document.getElementById(level.getAttribute("aria-controls") ?? "");
      if (!el) throw new Error("level options not open");
      return el;
    });
    await userEvent.click(within(options).getByRole("option", { name: "Редактирование", hidden: true }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ permissions: ["read", "write"] }));

    await userEvent.click(within(dialog).getByRole("button", { name: "Закрыть доступ для Боб" }));
    expect(await within(dialog).findByText("Пока ни у кого, кроме вас.")).toBeInTheDocument();
  });
});

describe("a folder shared with the user", () => {
  it("starts the breadcrumbs at the shared folder and offers only what the rights allow", async () => {
    mockBackend({
      "GET /api/v1/auth/me": me,
      // Read only, no resharing: the path begins at the shared folder.
      "GET /api/v1/nodes/s": () => Response.json({ ...node("s", "Проекты", "dir", ["read"]), path: [{ id: "s", name: "Проекты" }] }),
      "GET /api/v1/nodes/s/children": () => Response.json({ items: [node("f", "plan.docx", "file", ["read"])], nextCursor: null }),
    });
    renderApp("/files/s");
    const list = await screen.findByRole("listbox");
    await waitFor(() => expect(within(list).getAllByRole("option")).toHaveLength(1));
    // In the header, and as the breadcrumbs' first step.
    const back = screen.getAllByRole("link", { name: "Доступно мне" });
    expect(back).toHaveLength(2);
    for (const link of back) expect(link).toHaveAttribute("href", "/shared");
    expect(screen.queryByRole("button", { name: "Новая папка" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Загрузить" })).not.toBeInTheDocument();

    fireEvent.click(within(list).getAllByRole("option")[0]);
    await openSelectionActions();
    expect(await screen.findByRole("menuitem", { name: "Скачать" })).toBeInTheDocument();
    for (const name of ["Переименовать", "Переместить", "Удалить", "Поделиться"]) {
      expect(screen.queryByRole("menuitem", { name })).not.toBeInTheDocument();
    }
  });
});

describe("SharedPage", () => {
  it("lists what others shared, with owner and level", async () => {
    mockBackend({
      "GET /api/v1/auth/me": me,
      "GET /api/v1/shared-with-me": () =>
        Response.json({
          items: [
            { ...node("s", "Проекты", "dir", ["read", "write"]), owner: { type: "user", id: "u3", name: "Иван" }, sharedAt: mtime },
            { ...node("f", "отчёт.pdf", "file", ["read"]), owner: { type: "user", id: "u4", name: "Мария" }, sharedAt: mtime },
          ],
        }),
    });
    renderApp("/shared");
    expect(await screen.findByRole("link", { name: "Проекты" })).toHaveAttribute("href", "/files/s");
    expect(screen.getByText("Иван")).toBeInTheDocument();
    expect(screen.getByText("Редактирование")).toBeInTheDocument();
    expect(screen.getByText("Просмотр")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "отчёт.pdf" })).toBeInTheDocument();
  });

  it("opens a shared file like the file manager: editor, viewer, download", async () => {
    const opened = vi.spyOn(window, "open").mockImplementation(() => null);
    const clicked: string[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push(new URL(this.href).pathname);
    });
    const owner = { type: "user", id: "u3", name: "Иван" };
    mockBackend({
      "GET /api/v1/auth/me": me,
      "GET /api/v1/editor": () => Response.json({ extensions: { docx: "edit" } }),
      "GET /api/v1/shared-with-me": () =>
        Response.json({
          items: [
            { ...node("w", "план.docx", "file", ["read", "write"]), owner, sharedAt: mtime },
            { ...node("p", "отчёт.pdf", "file", ["read"]), owner, sharedAt: mtime },
            { ...node("z", "архив.zip", "file", ["read"]), owner, sharedAt: mtime },
          ],
        }),
    });
    renderApp("/shared");

    await userEvent.click(await screen.findByRole("button", { name: "план.docx" }));
    await waitFor(() => expect(opened).toHaveBeenCalledWith("/edit/w", "_blank", "noopener"));

    await userEvent.click(screen.getByRole("button", { name: "отчёт.pdf" }));
    const viewer = await screen.findByRole("dialog", { name: "отчёт.pdf" });
    await userEvent.click(within(viewer).getByRole("button", { name: "Закрыть" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    await userEvent.click(screen.getByRole("button", { name: "архив.zip" }));
    expect(clicked).toEqual(["/api/v1/nodes/z/content"]);
  });
});
