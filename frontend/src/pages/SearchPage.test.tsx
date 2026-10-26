import { afterEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { mockBackend, renderApp, type Call } from "../testing";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const mtime = "2026-10-24T10:00:00Z";
const ALL = ["read", "write", "reshare"];
const node = (id: string, name: string, kind: "dir" | "file" = "file", size = 1000) => ({ id, name, kind, size, etag: `e-${id}`, mtime, permissions: ALL });
const hit = (id: string, name: string, path: string, kind: "dir" | "file" = "file", parentId = "r") => ({ ...node(id, name, kind), path, parentId });

const alice = () => Response.json({ id: "u1", username: "alice", displayName: "Алиса", isAdmin: false });
const root = { ...node("r", "", "dir"), path: [{ id: "r", name: "" }] };

function backend(search: (url: URL) => Response, extra: Record<string, (url: URL) => Response> = {}): Call[] {
  return mockBackend({
    "GET /api/v1/auth/me": alice,
    "GET /api/v1/editor": () => Response.json({ extensions: { docx: "edit" } }),
    "GET /api/v1/search": search,
    "GET /api/v1/nodes/root": () => Response.json(root),
    "GET /api/v1/nodes/root/children": () => Response.json({ items: [], nextCursor: null }),
    ...extra,
  });
}

const searches = (calls: Call[]) => calls.filter((c) => c.url.pathname === "/api/v1/search");
// The header's field; the page has one of its own for narrow screens.
const headerField = () => within(screen.getByRole("banner")).getByRole("searchbox", { name: "Поиск файлов" });
const results = () => within(screen.getByRole("table")).getAllByRole("row").slice(1);

describe("header search", () => {
  it("opens /search on Enter and is focused by /", async () => {
    const calls = backend(() => Response.json({ items: [hit("a", "отчёт за год.txt", "Docs/2026")], nextCursor: null }));
    renderApp("/files");
    const field = await waitFor(headerField);

    // "/" from the page focuses it, without typing itself.
    await userEvent.keyboard("/");
    expect(field).toHaveFocus();
    expect(field).toHaveValue("");

    await userEvent.type(field, "отчёт за{Enter}");
    expect(await screen.findByText("отчёт за год.txt")).toBeInTheDocument();
    expect(searches(calls)[0].url.searchParams.get("q")).toBe("отчёт за");
    // On /search it keeps what was searched for.
    expect(headerField()).toHaveValue("отчёт за");
  });

  it("leaves / alone while typing in another field", async () => {
    backend(() => Response.json({ items: [], nextCursor: null }));
    renderApp("/search");
    const pageField = await within(screen.getByRole("main")).findByRole("searchbox", { name: "Поиск файлов" });
    await userEvent.click(pageField);
    await userEvent.keyboard("a/b");
    expect(pageField).toHaveFocus();
    expect(pageField).toHaveValue("a/b");
  });
});

describe("SearchPage", () => {
  it("asks for 3 characters without asking the server", async () => {
    const calls = backend(() => Response.json({ items: [], nextCursor: null }));
    renderApp("/search?q=%20ab%20");
    expect(await screen.findByText("Введите хотя бы 3 символа")).toBeInTheDocument();
    expect(screen.getByText(/Поиск идёт только по вашим файлам/)).toBeInTheDocument();
    expect(searches(calls)).toHaveLength(0);
  });

  it("shows results with their folders, and opens a folder", async () => {
    const calls = backend(
      () => Response.json({ items: [hit("d", "Отчёты", "", "dir"), hit("a", "отчёт.txt", "Docs/2026", "file", "y")], nextCursor: null }),
      {
        "GET /api/v1/nodes/y": () => Response.json({ ...node("y", "2026", "dir"), path: [{ id: "r", name: "" }, { id: "docs", name: "Docs" }, { id: "y", name: "2026" }] }),
        "GET /api/v1/nodes/y/children": () => Response.json({ items: [node("a", "отчёт.txt")], nextCursor: null }),
        "GET /api/v1/nodes/d": () => Response.json({ ...node("d", "Отчёты", "dir"), path: [{ id: "r", name: "" }, { id: "d", name: "Отчёты" }] }),
        "GET /api/v1/nodes/d/children": () => Response.json({ items: [], nextCursor: null }),
      },
    );
    renderApp("/search?q=%D0%BE%D1%82%D1%87");
    await waitFor(() => expect(results()).toHaveLength(2));
    expect(searches(calls)[0].url.searchParams.get("q")).toBe("отч");
    expect(screen.getByRole("link", { name: "Открыть папку «Мои файлы»" })).toHaveAttribute("href", "/files");
    expect(screen.getByRole("link", { name: "Открыть папку «Docs/2026»" })).toHaveAttribute("href", "/files/y");
    expect(screen.getByText(/Поиск идёт только по вашим файлам/)).toBeInTheDocument();

    // The folder column leads to the folder the result is in.
    await userEvent.click(screen.getByRole("link", { name: "Открыть папку «Docs/2026»" }));
    expect(await screen.findByRole("listbox")).toBeInTheDocument();
    expect(calls.some((c) => c.url.pathname === "/api/v1/nodes/y/children")).toBe(true);
  });

  it("opens a found folder", async () => {
    const calls = backend(() => Response.json({ items: [hit("d", "Отчёты", "", "dir")], nextCursor: null }), {
      "GET /api/v1/nodes/d": () => Response.json({ ...node("d", "Отчёты", "dir"), path: [{ id: "r", name: "" }, { id: "d", name: "Отчёты" }] }),
      "GET /api/v1/nodes/d/children": () => Response.json({ items: [], nextCursor: null }),
    });
    renderApp("/search?q=Отч");
    await userEvent.click(await screen.findByRole("link", { name: "Отчёты" }));
    expect(await screen.findByText("Папка пуста")).toBeInTheDocument();
    expect(calls.some((c) => c.url.pathname === "/api/v1/nodes/d/children")).toBe(true);
  });

  it("says when nothing is found", async () => {
    backend(() => Response.json({ items: [], nextCursor: null }));
    renderApp("/search?q=zzzz");
    expect(await screen.findByText("Ничего не найдено по запросу «zzzz»")).toBeInTheDocument();
  });

  it("shows an error with a retry", async () => {
    let fail = true;
    backend(() => (fail ? Response.json({ code: "internal", message: "boom" }, { status: 500 }) : Response.json({ items: [hit("a", "abc.txt", "")], nextCursor: null })));
    renderApp("/search?q=abc");
    const retry = await screen.findByRole("button", { name: "Повторить" });
    fail = false;
    await userEvent.click(retry);
    expect(await screen.findByText("abc.txt")).toBeInTheDocument();
  });

  it("loads the next page with the cursor", async () => {
    let failMore = true;
    const calls = backend((url) => {
      if (!url.searchParams.get("cursor")) return Response.json({ items: [hit("a", "abc-1.txt", "")], nextCursor: "c1" });
      if (failMore) return Response.json({ code: "internal", message: "boom" }, { status: 500 });
      return Response.json({ items: [hit("b", "abc-2.txt", "")], nextCursor: null });
    });
    renderApp("/search?q=abc");
    await userEvent.click(await screen.findByRole("button", { name: "Показать ещё" }));
    // A failed page keeps what's loaded and offers the same request again.
    const retry = await screen.findByRole("button", { name: "Повторить" });
    expect(screen.getByText("abc-1.txt")).toBeInTheDocument();
    failMore = false;
    await userEvent.click(retry);
    expect(await screen.findByText("abc-2.txt")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Показать ещё" })).not.toBeInTheDocument();
    expect(searches(calls).map((c) => c.url.searchParams.get("cursor"))).toEqual([null, "c1", "c1"]);
  });

  it("opens files like the file manager: viewer, editor, download", async () => {
    const opened = vi.spyOn(window, "open").mockImplementation(() => null);
    const clicked: string[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push(new URL(this.href).pathname);
    });
    backend(
      () =>
        Response.json({
          items: [hit("i", "abc.jpg", "Фото"), hit("t", "abc.txt", ""), hit("p", "abc.pdf", "Docs"), hit("w", "abc.docx", "Docs"), hit("z", "abc.zip", "")],
          nextCursor: null,
        }),
      { "GET /api/v1/nodes/t/content": () => new Response("abc inside") },
    );
    renderApp("/search?q=abc");
    await waitFor(() => expect(results()).toHaveLength(5));

    // The viewer steps through the viewable results only.
    await userEvent.click(screen.getByRole("button", { name: "abc.jpg" }));
    let viewer = await screen.findByRole("dialog", { name: "abc.jpg" });
    expect(within(viewer).getByRole("img", { name: "abc.jpg" })).toHaveAttribute("src", "/api/v1/nodes/i/preview?size=l&v=e-i");
    expect(within(viewer).getByText("1 из 3")).toBeInTheDocument();
    await userEvent.click(within(viewer).getByRole("button", { name: "Закрыть" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    // Text is shown too.
    await userEvent.click(screen.getByRole("button", { name: "abc.txt" }));
    viewer = await screen.findByRole("dialog", { name: "abc.txt" });
    expect(await within(viewer).findByText("abc inside")).toBeInTheDocument();
    await userEvent.click(within(viewer).getByRole("button", { name: "Закрыть" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    await userEvent.click(screen.getByRole("button", { name: "abc.zip" }));
    expect(clicked).toEqual(["/api/v1/nodes/z/content"]);

    // Documents get a tab of their own; the results stay.
    await userEvent.click(screen.getByRole("button", { name: "abc.docx" }));
    expect(opened).toHaveBeenCalledWith("/edit/w", "_blank", "noopener");
    expect(results()).toHaveLength(5);
  });
});
