import { afterEach, beforeAll, afterAll, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { COARSE_MEDIA, PHONE_MEDIA } from "../media";
import { currentUrl, mockBackend, renderApp, selectionAction, stubMedia, unauthorized, type Call } from "../testing";

// jsdom lays nothing out, so every element measures 0×0 and the
// virtualizer would render no rows. Give elements a screen-sized box (the
// grid counts its columns by clientWidth).
const sizes = { offsetHeight: 600, offsetWidth: 800, clientWidth: 800 };
const saved = Object.fromEntries(Object.keys(sizes).map((k) => [k, Object.getOwnPropertyDescriptor(HTMLElement.prototype, k)]));
beforeAll(() => {
  for (const [k, v] of Object.entries(sizes)) Object.defineProperty(HTMLElement.prototype, k, { configurable: true, value: v });
});
afterAll(() => {
  for (const [k, d] of Object.entries(saved)) if (d) Object.defineProperty(HTMLElement.prototype, k, d);
});
afterEach(() => {
  vi.unstubAllGlobals();
  // The view and the panel are remembered: each test starts afresh.
  localStorage.clear();
});

const alice = { id: "u1", username: "alice", displayName: "Алиса", isAdmin: false };
const mtime = "2026-10-24T10:00:00Z";
// The owner's rights: everything.
const ALL = ["read", "write", "reshare"];
const node = (id: string, name: string, kind: "dir" | "file" = "file", size = 1) => ({ id, name, kind, size, etag: "e", mtime, permissions: ALL });

const root = { ...node("r", "", "dir"), path: [{ id: "r", name: "" }] };
const docs = { ...node("d", "Docs", "dir"), path: [{ id: "r", name: "" }, { id: "d", name: "Docs" }] };
const rootItems = [node("d", "Docs", "dir"), node("a", "a.txt"), node("b", "b.txt"), node("c", "c.txt")];

function backend(extra: Record<string, (url: URL) => Response> = {}): Call[] {
  return mockBackend({
    "GET /api/v1/auth/me": () => Response.json(alice),
    "GET /api/v1/nodes/root": () => Response.json(root),
    "GET /api/v1/nodes/root/children": () => Response.json({ items: rootItems, nextCursor: null }),
    "GET /api/v1/nodes/d": () => Response.json(docs),
    "GET /api/v1/nodes/d/children": () => Response.json({ items: [node("x", "inside.txt")], nextCursor: null }),
    ...extra,
  });
}

const serverError = () => Response.json({ code: "internal_error", message: "boom" }, { status: 500 });

// The folder's items, without the ".." row a subfolder starts with.
const upRow = () => within(screen.getByRole("listbox")).getByRole("option", { name: "Наверх" });
const rows = () =>
  within(screen.getByRole("listbox"))
    .getAllByRole("option")
    .filter((r) => r.getAttribute("aria-label") !== "Наверх");
const names = () => rows().map((r) => r.textContent ?? "");

describe("FilesPage", () => {
  it("lists a folder, folders first as the backend sends them, and opens a folder", async () => {
    backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    expect(names()[0]).toContain("Docs");
    expect(screen.getByText("Мои файлы")).toBeInTheDocument();

    fireEvent.doubleClick(rows()[0]);
    expect(await screen.findByText("inside.txt")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Мои файлы" })).toHaveAttribute("href", "/files");
    expect(screen.getByText("Docs")).toBeInTheDocument();
  });

  it("sorts by a column, flipping the order on a second click", async () => {
    const calls = backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    const lastList = () => {
      const lists = calls.filter((c) => c.url.pathname === "/api/v1/nodes/root/children");
      return lists[lists.length - 1]?.url.searchParams;
    };

    await userEvent.click(screen.getByRole("button", { name: "Сортировать по столбцу «Размер»" }));
    await waitFor(() => expect(lastList()?.get("sort")).toBe("size"));
    expect(lastList()?.get("order")).toBe("desc");
    await userEvent.click(screen.getByRole("button", { name: "Сортировать по столбцу «Размер»" }));
    await waitFor(() => expect(lastList()?.get("order")).toBe("asc"));
  });

  it("selects with click, Ctrl+click, Shift+click and the keyboard", async () => {
    backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    const selected = () => rows().filter((r) => r.getAttribute("aria-selected") === "true").map((r) => r.id);

    fireEvent.click(rows()[1]);
    expect(selected()).toEqual(["node-a"]);
    fireEvent.click(rows()[3], { ctrlKey: true });
    expect(selected()).toEqual(["node-a", "node-c"]);
    fireEvent.click(rows()[3], { shiftKey: true });
    expect(selected()).toEqual(["node-c"]);
    fireEvent.click(rows()[1]);
    fireEvent.click(rows()[3], { shiftKey: true });
    expect(selected()).toEqual(["node-a", "node-b", "node-c"]);
    expect(screen.getByText("Выбрано: 3")).toBeInTheDocument();

    const list = screen.getByRole("listbox");
    fireEvent.keyDown(list, { key: "Escape" });
    expect(selected()).toEqual([]);
    fireEvent.keyDown(list, { key: "a", ctrlKey: true });
    expect(selected()).toHaveLength(4);
    fireEvent.keyDown(list, { key: "Home" });
    expect(selected()).toEqual(["node-d"]);
    fireEvent.keyDown(list, { key: "Enter" });
    expect(await screen.findByText("inside.txt")).toBeInTheDocument();
  });

  it("keeps the focus on the list going into a folder and back up from the keyboard", async () => {
    backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    const list = screen.getByRole("listbox");
    list.focus();
    fireEvent.keyDown(list, { key: "Enter" });
    await waitFor(() => expect(names()).toEqual([expect.stringContaining("inside.txt")]));
    expect(document.activeElement).toBe(screen.getByRole("listbox"));
    // The first row is active on arrival: "..", which the arrows reach too
    // and Enter follows.
    expect(screen.getByRole("listbox")).toHaveAttribute("aria-activedescendant", "node-up");
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "ArrowDown" });
    expect(screen.getByRole("listbox")).toHaveAttribute("aria-activedescendant", "node-x");
    expect(rows()[0]).toHaveAttribute("aria-selected", "true");
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "ArrowUp" });
    expect(screen.getByRole("listbox")).toHaveAttribute("aria-activedescendant", "node-up");
    expect(rows()[0]).toHaveAttribute("aria-selected", "false");
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "Enter" });
    await waitFor(() => expect(rows()).toHaveLength(4));
    // At the top, with no "..", the first item.
    expect(document.activeElement).toBe(screen.getByRole("listbox"));
    expect(screen.getByRole("listbox")).toHaveAttribute("aria-activedescendant", "node-d");

    fireEvent.keyDown(screen.getByRole("listbox"), { key: "Enter" });
    await screen.findByText("inside.txt");
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "Backspace" });
    await waitFor(() => expect(rows()).toHaveLength(4));
    expect(screen.getByRole("listbox")).toHaveAttribute("aria-activedescendant", "node-d");

    // Alt+ArrowUp goes up too; at the top there is nowhere to go.
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "Enter" });
    await screen.findByText("inside.txt");
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "ArrowUp", altKey: true });
    await waitFor(() => expect(rows()).toHaveLength(4));
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "Backspace" });
    expect(rows()).toHaveLength(4);
    expect(screen.getByText("Мои файлы")).toBeInTheDocument();
  });

  it("leaves the focus alone opening a folder with the mouse", async () => {
    backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    fireEvent.doubleClick(rows()[0]);
    expect(await screen.findByText("inside.txt")).toBeInTheDocument();
    expect(document.activeElement).not.toBe(screen.getByRole("listbox"));
  });

  it("loads the next page through the cursor", async () => {
    const calls = backend({
      "GET /api/v1/nodes/root/children": () => Response.json({ items: rootItems, nextCursor: "next-1" }),
    });
    // The second page comes from the same route; tell them apart by cursor.
    const original = globalThis.fetch;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input), "http://localhost");
        if (url.searchParams.get("cursor") === "next-1") {
          calls.push({ method: "GET", url, body: undefined });
          return Response.json({ items: [node("z", "zzz.txt")], nextCursor: null });
        }
        return original(input, init);
      }),
    );
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(5));
    expect(names()[4]).toContain("zzz.txt");
    expect(calls.filter((c) => c.url.searchParams.get("cursor") === "next-1")).toHaveLength(1);
  });

  it("shows an empty folder", async () => {
    backend({ "GET /api/v1/nodes/root/children": () => Response.json({ items: [], nextCursor: null }) });
    renderApp("/files");
    expect(await screen.findByText("Здесь появятся ваши файлы")).toBeInTheDocument();
  });

  it("explains a folder that doesn't exist", async () => {
    backend();
    renderApp("/files/gone");
    expect(await screen.findByText(/Папка не найдена/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "К моим файлам" })).toHaveAttribute("href", "/files");
  });

  it("goes to the login page when the session ends", async () => {
    let live = true;
    mockBackend({
      "GET /api/v1/auth/me": () => (live ? Response.json(alice) : unauthorized()),
      "GET /api/v1/auth/providers": () => Response.json({ password: true, providers: [] }),
      "GET /api/v1/nodes/root": () => Response.json(root),
      "GET /api/v1/nodes/root/children": () => Response.json({ items: rootItems, nextCursor: null }),
      "GET /api/v1/nodes/d": () => {
        live = false;
        return unauthorized();
      },
      "GET /api/v1/nodes/d/children": unauthorized,
    });
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    fireEvent.doubleClick(rows()[0]);
    expect(await screen.findByText("Вход в Облако")).toBeInTheDocument();
  });
});

describe("FilesPage failures", () => {
  const listings = (calls: Call[]) => calls.filter((c) => c.url.pathname === "/api/v1/nodes/root/children");

  it("offers a retry when the folder fails to load", async () => {
    let fail = true;
    backend({ "GET /api/v1/nodes/root/children": () => (fail ? serverError() : Response.json({ items: rootItems, nextCursor: null })) });
    renderApp("/files");
    expect(await screen.findByText("Не удалось загрузить")).toBeInTheDocument();
    expect(screen.getByText("Внутренняя ошибка сервера.")).toBeInTheDocument();
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Повторить" }));
    await waitFor(() => expect(rows()).toHaveLength(4));
  });

  it("keeps the list when the next page fails and asks again only on retry", async () => {
    let failMore = true;
    const calls = backend({
      "GET /api/v1/nodes/root/children": (url) => {
        if (url.searchParams.get("cursor") !== "next-1") return Response.json({ items: rootItems, nextCursor: "next-1" });
        return failMore ? serverError() : Response.json({ items: [node("z", "zzz.txt")], nextCursor: null });
      },
    });
    const more = () => listings(calls).filter((c) => c.url.searchParams.get("cursor") === "next-1").length;
    renderApp("/files");
    expect(await screen.findByText("Не удалось загрузить продолжение")).toBeInTheDocument();
    expect(rows()).toHaveLength(4);
    fireEvent.click(rows()[1]);
    // Time for a re-firing auto-load to show up as more requests.
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(more()).toBe(1);

    failMore = false;
    await userEvent.click(screen.getByRole("button", { name: "Повторить" }));
    await waitFor(() => expect(names()[4]).toContain("zzz.txt"));
    expect(more()).toBe(2);
    expect(rows()[1]).toHaveAttribute("aria-selected", "true");
  });

  it("keeps showing the folder when a refetch fails", async () => {
    let down = false;
    const calls = backend({
      "GET /api/v1/nodes/root/children": () => (down ? serverError() : Response.json({ items: rootItems, nextCursor: null })),
      "POST /api/v1/nodes/r/children": () => Response.json(node("n", "Новая", "dir"), { status: 201 }),
    });
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    down = true;
    // Creating a folder refetches the listing, which now fails.
    await userEvent.click(screen.getByRole("button", { name: "Новая папка" }));
    await userEvent.type(await screen.findByRole("textbox", { name: "Имя" }), "Новая");
    await userEvent.click(screen.getByRole("button", { name: "Создать" }));
    await waitFor(() => expect(listings(calls)).toHaveLength(2));
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(rows()).toHaveLength(4);
    expect(screen.queryByText("Не удалось загрузить")).not.toBeInTheDocument();
  });
});

// A folder whose contents change as the actions under test hit the fake
// backend, the way the real one would.
function statefulBackend() {
  const folders: Record<string, ReturnType<typeof node>[]> = {
    r: [node("d", "Docs", "dir"), node("a", "a.txt"), node("b", "b.txt")],
    d: [],
  };
  const trash: Record<string, ReturnType<typeof node>> = {};
  const parentOf = (id: string) => (folders.r.some((n) => n.id === id) ? "r" : "d");
  const take = (id: string) => {
    const from = parentOf(id);
    const n = folders[from].find((x) => x.id === id);
    folders[from] = folders[from].filter((x) => x.id !== id);
    return n;
  };
  const list = (dir: string) => () => Response.json({ items: folders[dir], nextCursor: null });
  const calls = mockBackend({
    "GET /api/v1/auth/me": () => Response.json(alice),
    "GET /api/v1/nodes/root": () => Response.json(root),
    "GET /api/v1/nodes/r": () => Response.json(root),
    "GET /api/v1/nodes/d": () => Response.json(docs),
    "GET /api/v1/nodes/root/children": list("r"),
    "GET /api/v1/nodes/r/children": list("r"),
    "GET /api/v1/nodes/d/children": list("d"),
    "POST /api/v1/nodes/r/children": () => {
      const n = node("new", "Новая", "dir");
      folders.r.push(n);
      return Response.json(n, { status: 201 });
    },
    "PATCH /api/v1/nodes/a": () => {
      const n = folders.r.find((x) => x.id === "a");
      if (n) n.name = "renamed.txt";
      return Response.json(n);
    },
    "POST /api/v1/nodes/batch": () => {
      const body = calls[calls.length - 1].body;
      const ops = typeof body === "object" && body !== null && "ops" in body && Array.isArray(body.ops) ? body.ops : [];
      const results = ops.map((op: { op: string; id: string; parentId?: string }) => {
        if (op.id === "b" && op.op === "move") return { id: op.id, error: { code: "name_conflict", message: "taken" } };
        const n = take(op.id);
        if (!n) return { id: op.id, error: { code: "not_found", message: "gone" } };
        if (op.op === "delete") trash[n.id] = n;
        else folders[op.parentId === "d" ? "d" : "r"].push(n);
        return op.op === "delete" ? { id: op.id } : { id: op.id, node: n };
      });
      return Response.json({ results });
    },
    "POST /api/v1/trash/a/restore": () => {
      folders.r.push(trash.a);
      return Response.json(trash.a);
    },
  });
  return { calls, folders };
}

describe("FilesPage actions", () => {
  it("creates a folder", async () => {
    const { calls } = statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    await userEvent.click(screen.getByRole("button", { name: "Новая папка" }));
    await userEvent.type(await screen.findByRole("textbox", { name: "Имя" }), "Новая");
    await userEvent.click(screen.getByRole("button", { name: "Создать" }));
    await waitFor(() => expect(rows()).toHaveLength(4));
    expect(calls.find((c) => c.method === "POST" && c.url.pathname === "/api/v1/nodes/r/children")?.body).toEqual({ name: "Новая" });
  });

  it("renames with F2, preselecting the name without its extension", async () => {
    const { calls } = statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    fireEvent.click(rows()[1]);
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "F2" });
    const input = await screen.findByRole<HTMLInputElement>("textbox", { name: "Имя" });
    await waitFor(() => expect([input.selectionStart, input.selectionEnd]).toEqual([0, 1]));
    await userEvent.clear(input);
    await userEvent.type(input, "renamed.txt");
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Переименовать" }));
    expect(await screen.findByText("renamed.txt")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ name: "renamed.txt" });
  });

  it("deletes the selection and undoes it", async () => {
    statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    fireEvent.click(rows()[1]);
    await selectionAction("Удалить");
    expect(await screen.findByText("Перемещено в корзину: 1")).toBeInTheDocument();
    await waitFor(() => expect(rows()).toHaveLength(2));
    await userEvent.click(screen.getByRole("button", { name: "Отменить" }));
    await waitFor(() => expect(rows()).toHaveLength(3));
  });

  it("moves through the folder picker and reports what failed", async () => {
    const { folders } = statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    fireEvent.click(rows()[1]);
    fireEvent.click(rows()[2], { shiftKey: true });
    await selectionAction("Переместить");
    const dialog = await screen.findByRole("dialog");
    // Moving into the folder they are in is a no-op, so not offered.
    expect(within(dialog).getByRole("button", { name: "Переместить сюда" })).toBeDisabled();
    await userEvent.click(await within(dialog).findByText("Docs"));
    await waitFor(() => expect(within(dialog).getByRole("button", { name: "Переместить сюда" })).toBeEnabled());
    await userEvent.click(within(dialog).getByRole("button", { name: "Переместить сюда" }));
    expect(await screen.findByText("Перемещено: 1")).toBeInTheDocument();
    expect(screen.getByText(/Не удалось: 1\. Здесь уже есть/)).toBeInTheDocument();
    expect(folders.d.map((n) => n.name)).toEqual(["a.txt"]);
  });

  it("downloads a file, or a folder as ZIP", async () => {
    statefulBackend();
    const clicked: string[] = [];
    const spy = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push(new URL(this.href).pathname);
    });
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    // A double click on a text file views it now: the toolbar downloads.
    fireEvent.click(rows()[1]);
    await selectionAction("Скачать");
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "Escape" });
    await userEvent.click(screen.getByRole("button", { name: "Скачать папку" }));
    fireEvent.click(rows()[0]);
    await selectionAction("Скачать");
    expect(clicked).toEqual(["/api/v1/nodes/a/content", "/api/v1/nodes/r/archive", "/api/v1/nodes/d/archive"]);
    spy.mockRestore();
  });
});

const isSelected = (r: HTMLElement) => r.getAttribute("aria-selected") === "true";
const selectedIds = () => rows().filter(isSelected).map((r) => r.id);
// The menu opens at a point that jsdom, laying nothing out, reports as off
// screen, so Mantine keeps it display: none: queries include hidden ones.
const menu = () => screen.findByRole("menu", { hidden: true });
const menuItems = async () => within(await menu()).getAllByRole("menuitem", { hidden: true }).map((i) => i.textContent);
const menuItem = async (name: string) => within(await menu()).getByRole("menuitem", { hidden: true, name });

describe("FilesPage context menu", () => {
  it("selects the right-clicked row alone and offers its actions", async () => {
    statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    fireEvent.click(rows()[2]);
    fireEvent.contextMenu(rows()[1], { button: 2 });
    expect(selectedIds()).toEqual(["node-a"]);
    expect(await menuItems()).toEqual(["Скачать", "Версии", "Поделиться", "Переименовать", "Копировать", "Переместить", "Удалить"]);

    fireEvent.click(await menuItem("Переименовать"));
    expect(await screen.findByRole("textbox", { name: "Имя" })).toHaveValue("a.txt");
  });

  it("deletes from the menu", async () => {
    const { calls } = statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    fireEvent.contextMenu(rows()[1], { button: 2 });
    fireEvent.click(await menuItem("Удалить"));
    expect(await screen.findByText("Перемещено в корзину: 1")).toBeInTheDocument();
    expect(calls.find((c) => c.url.pathname === "/api/v1/nodes/batch")?.body).toEqual({ ops: [{ op: "delete", id: "a" }] });
  });

  it("keeps the selection a right-clicked row is in", async () => {
    statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    fireEvent.click(rows()[1]);
    fireEvent.click(rows()[2], { ctrlKey: true });
    fireEvent.contextMenu(rows()[2], { button: 2 });
    expect(selectedIds()).toEqual(["node-a", "node-b"]);
    // Only what works on several at once.
    expect(await menuItems()).toEqual(["Копировать", "Переместить", "Удалить"]);
  });

  it("toggles on Ctrl+click as a Mac sends it, without a menu", async () => {
    statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    fireEvent.click(rows()[1]);
    const event = fireEvent.contextMenu(rows()[2], { button: 0, ctrlKey: true });
    // fireEvent returns false when the default (the system menu) was prevented.
    expect(event).toBe(false);
    expect(selectedIds()).toEqual(["node-a", "node-b"]);
    fireEvent.contextMenu(rows()[1], { button: 0, ctrlKey: true });
    expect(selectedIds()).toEqual(["node-b"]);
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.queryByRole("menu", { hidden: true })).not.toBeInTheDocument();
  });

  it("opens from the keyboard for the active row", async () => {
    statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    const list = screen.getByRole("listbox");
    fireEvent.keyDown(list, { key: "Home" });
    fireEvent.keyDown(list, { key: "ArrowDown" });
    fireEvent.keyDown(list, { key: "F10", shiftKey: true });
    expect(selectedIds()).toEqual(["node-a"]);
    expect(await menuItems()).toContain("Переименовать");
  });

  it("offers the folder's actions below the rows", async () => {
    statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    fireEvent.click(rows()[1]);
    fireEvent.contextMenu(screen.getByRole("listbox"), { button: 2 });
    expect(selectedIds()).toEqual([]);
    expect(await menuItems()).toEqual(["Скачать папку", "Новая папка", "Загрузить"]);
    fireEvent.click(await menuItem("Новая папка"));
    expect(await screen.findByRole("textbox", { name: "Имя" })).toBeInTheDocument();
  });

  it("offers the folder's actions in an empty folder", async () => {
    statefulBackend();
    renderApp("/files/d");
    fireEvent.contextMenu(await screen.findByText("Папка пуста"), { button: 2 });
    expect(await menuItems()).toEqual(["Новая папка", "Загрузить"]);
  });
});

// jsdom has no DataTransfer; this holds what a drag carries the way the
// browser's does.
function fakeDataTransfer() {
  const data = new Map<string, string>();
  return {
    get types() {
      return [...data.keys()];
    },
    setData: (type: string, value: string) => data.set(type, value),
    getData: (type: string) => data.get(type) ?? "",
    setDragImage: vi.fn(),
    effectAllowed: "uninitialized",
    dropEffect: "none",
  };
}

function dragOnto(from: HTMLElement, to: HTMLElement) {
  const dataTransfer = fakeDataTransfer();
  fireEvent.dragStart(from, { dataTransfer });
  fireEvent.dragEnter(to, { dataTransfer });
  fireEvent.dragOver(to, { dataTransfer });
  fireEvent.drop(to, { dataTransfer });
  fireEvent.dragEnd(from, { dataTransfer });
  return dataTransfer;
}

const batches = (calls: Call[]) => calls.filter((c) => c.url.pathname === "/api/v1/nodes/batch").map((c) => c.body);

describe("FilesPage up row", () => {
  it("starts a subfolder with .., even an empty one, and goes up by it", async () => {
    backend({
      "GET /api/v1/nodes/d/children": () => Response.json({ items: [], nextCursor: null }),
    });
    renderApp("/files/d");
    expect(await screen.findByText("Папка пуста")).toBeInTheDocument();
    expect(rows()).toHaveLength(0);
    fireEvent.doubleClick(upRow());
    await waitFor(() => expect(currentUrl()).toBe("/files"));
    await waitFor(() => expect(rows()).toHaveLength(4));
    expect(within(screen.getByRole("listbox")).queryByRole("option", { name: "Наверх" })).not.toBeInTheDocument();
  });
});

describe("FilesPage drag and drop", () => {
  it("moves a dragged row onto a folder row", async () => {
    const { calls, folders } = statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    dragOnto(rows()[1], rows()[0]);
    expect(await screen.findByText("Перемещено: 1")).toBeInTheDocument();
    expect(batches(calls)).toEqual([{ ops: [{ op: "move", id: "a", parentId: "d" }] }]);
    expect(folders.d.map((n) => n.name)).toEqual(["a.txt"]);
  });

  it("drags the whole selection from a selected row, with a stacked image and a count", async () => {
    const { calls } = statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    const image = (dataTransfer: ReturnType<typeof fakeDataTransfer>) => {
      const [el, x, y] = dataTransfer.setDragImage.mock.lastCall ?? [];
      if (!(el instanceof HTMLElement)) throw new Error("no drag image");
      // Off the pointer's corner by the image's own padding, not by offsets.
      expect([x, y]).toEqual([0, 0]);
      return el;
    };
    // One row: its card alone, no count.
    const one = fakeDataTransfer();
    fireEvent.dragStart(rows()[1], { dataTransfer: one });
    expect(image(one).textContent).toBe("TXTa.txt");
    fireEvent.dragEnd(rows()[1], { dataTransfer: one });

    fireEvent.click(rows()[1]);
    fireEvent.click(rows()[2], { ctrlKey: true });
    const dataTransfer = dragOnto(rows()[2], rows()[0]);
    await waitFor(() => expect(batches(calls)).toHaveLength(1));
    expect(batches(calls)[0]).toEqual({
      ops: [
        { op: "move", id: "a", parentId: "d" },
        { op: "move", id: "b", parentId: "d" },
      ],
    });
    expect(dataTransfer.setDragImage).toHaveBeenCalledOnce();
    // The dragged row's card is drawn last, on top; the count over them.
    const cards = Array.from(image(dataTransfer).firstElementChild?.children ?? []).map((c) => c.textContent);
    expect(cards).toEqual(["TXTa.txt", "TXTb.txt", "Элементов: 2"]);
  });

  it("highlights a folder row while a drag is over it, and not the dragged folder", async () => {
    const { calls } = statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    const dataTransfer = fakeDataTransfer();
    fireEvent.dragStart(rows()[1], { dataTransfer });
    fireEvent.dragEnter(rows()[0], { dataTransfer });
    expect(rows()[0]).toHaveAttribute("data-drop-target");
    fireEvent.dragLeave(rows()[0], { dataTransfer });
    expect(rows()[0]).not.toHaveAttribute("data-drop-target");
    fireEvent.dragEnd(rows()[1], { dataTransfer });

    // A folder can't go into itself: no highlight, no drop, no request.
    const self = fakeDataTransfer();
    fireEvent.dragStart(rows()[0], { dataTransfer: self });
    fireEvent.dragEnter(rows()[0], { dataTransfer: self });
    expect(rows()[0]).not.toHaveAttribute("data-drop-target");
    expect(fireEvent.dragOver(rows()[0], { dataTransfer: self })).toBe(true);
    fireEvent.drop(rows()[0], { dataTransfer: self });
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(batches(calls)).toEqual([]);
  });

  it("moves up onto a folder in the breadcrumbs", async () => {
    const calls = backend({
      "POST /api/v1/nodes/batch": () => Response.json({ results: [{ id: "x", node: node("x", "inside.txt") }] }),
    });
    renderApp("/files/d");
    await waitFor(() => expect(rows()).toHaveLength(1));
    const crumb = screen.getByRole("link", { name: "Мои файлы" });
    const dataTransfer = fakeDataTransfer();
    fireEvent.dragStart(rows()[0], { dataTransfer });
    fireEvent.dragOver(crumb, { dataTransfer });
    expect(crumb).toHaveAttribute("data-drop-target");
    fireEvent.drop(crumb, { dataTransfer });
    await waitFor(() => expect(batches(calls)).toEqual([{ ops: [{ op: "move", id: "x", parentId: "r" }] }]));
    expect(crumb).not.toHaveAttribute("data-drop-target");
  });

  it("isn't taken for an upload", async () => {
    statefulBackend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    const dataTransfer = fakeDataTransfer();
    fireEvent.dragStart(rows()[1], { dataTransfer });
    fireEvent.dragEnter(screen.getByRole("listbox"), { dataTransfer });
    fireEvent.dragEnter(rows()[2], { dataTransfer });
    expect(screen.queryByText("Отпустите, чтобы загрузить сюда")).not.toBeInTheDocument();
  });
});

describe("FilesPage grid view", () => {
  const lastList = (calls: Call[]) => {
    const lists = calls.filter((c) => c.url.pathname === "/api/v1/nodes/root/children");
    return lists[lists.length - 1]?.url.searchParams;
  };

  it("switches to tiles, remembers that, and sorts from a menu", async () => {
    const calls = backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    expect(screen.getByRole("button", { name: "Сортировать по столбцу «Размер»" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Плитка" }));
    expect(screen.getByRole("button", { name: "Плитка" })).toHaveAttribute("aria-pressed", "true");
    // The same options, as tiles: no column headers, a sort menu instead.
    expect(rows()).toHaveLength(4);
    expect(within(rows()[1]).getByTitle("a.txt")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Сортировать по столбцу «Размер»" })).not.toBeInTheDocument();
    expect(localStorage.getItem("cloud.files.view")).toBe("grid");

    await userEvent.click(screen.getByRole("button", { name: "Сортировка" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Размер" }));
    await waitFor(() => expect(lastList(calls)?.get("sort")).toBe("size"));
    expect(lastList(calls)?.get("order")).toBe("desc");
    await userEvent.click(screen.getByRole("button", { name: "Сортировка" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "По возрастанию" }));
    await waitFor(() => expect(lastList(calls)?.get("order")).toBe("asc"));
    expect(lastList(calls)?.get("sort")).toBe("size");

    // Tile size, remembered too.
    await userEvent.click(screen.getByText("L"));
    expect(localStorage.getItem("cloud.files.tileSize")).toBe("l");

    // A fresh page starts in tiles.
    cleanup();
    backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    expect(screen.getByRole("button", { name: "Плитка" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.queryByRole("button", { name: "Сортировать по столбцу «Имя»" })).not.toBeInTheDocument();
  });

  it("moves across tiles with left and right, between lines with up and down", async () => {
    // 800 px of 180 px tiles: four to a line.
    localStorage.setItem("cloud.files.view", "grid");
    const many = Array.from({ length: 10 }, (_, i) => node(`n${i}`, `file${i}.bin`));
    backend({ "GET /api/v1/nodes/root/children": () => Response.json({ items: many, nextCursor: null }) });
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(10));
    const list = screen.getByRole("listbox");
    const selected = () => rows().filter((r) => r.getAttribute("aria-selected") === "true").map((r) => r.id);

    fireEvent.keyDown(list, { key: "Home" });
    expect(selected()).toEqual(["node-n0"]);
    fireEvent.keyDown(list, { key: "ArrowRight" });
    expect(selected()).toEqual(["node-n1"]);
    fireEvent.keyDown(list, { key: "ArrowDown" });
    expect(selected()).toEqual(["node-n5"]);
    fireEvent.keyDown(list, { key: "ArrowLeft" });
    expect(selected()).toEqual(["node-n4"]);
    fireEvent.keyDown(list, { key: "ArrowUp" });
    expect(selected()).toEqual(["node-n0"]);
    fireEvent.keyDown(list, { key: "ArrowDown", shiftKey: true });
    expect(selected()).toEqual(["node-n0", "node-n1", "node-n2", "node-n3", "node-n4"]);
    fireEvent.keyDown(list, { key: "End" });
    expect(selected()).toEqual(["node-n9"]);
    // Selection and opening work on tiles as on rows.
    fireEvent.click(rows()[2], { ctrlKey: true });
    expect(selected()).toEqual(["node-n2", "node-n9"]);
  });

  it("makes '..' the first tile, reached by the arrows like the others", async () => {
    // Four to a line: "..", n0, n1, n2 / n3 … n6 / n7 … n9.
    localStorage.setItem("cloud.files.view", "grid");
    const many = Array.from({ length: 10 }, (_, i) => node(`n${i}`, `file${i}.bin`));
    backend({ "GET /api/v1/nodes/d/children": () => Response.json({ items: many, nextCursor: null }) });
    renderApp("/files/d");
    await waitFor(() => expect(rows()).toHaveLength(10));
    const list = screen.getByRole("listbox");
    const selected = () => rows().filter((r) => r.getAttribute("aria-selected") === "true").map((r) => r.id);
    // A tile in the first line, not a row above the grid.
    expect(upRow().parentElement).toBe(document.getElementById("node-n0")?.parentElement);

    fireEvent.keyDown(list, { key: "Home" });
    expect(list).toHaveAttribute("aria-activedescendant", "node-up");
    fireEvent.keyDown(list, { key: "ArrowRight" });
    expect(selected()).toEqual(["node-n0"]);
    fireEvent.keyDown(list, { key: "ArrowDown" });
    expect(selected()).toEqual(["node-n4"]);
    fireEvent.keyDown(list, { key: "ArrowUp" });
    expect(selected()).toEqual(["node-n0"]);
    fireEvent.keyDown(list, { key: "ArrowLeft" });
    expect(list).toHaveAttribute("aria-activedescendant", "node-up");
    expect(selected()).toEqual([]);
    fireEvent.keyDown(list, { key: "ArrowDown" });
    expect(selected()).toEqual(["node-n3"]);
  });
});

describe("FilesPage shared items", () => {
  it("marks what is shared in the list and the details panel", async () => {
    vi.stubGlobal("matchMedia", (query: string) => ({
      matches: query === "(min-width: 900px)",
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }));
    localStorage.setItem("cloud.files.panel", "1");
    const items = [{ ...node("d", "Docs", "dir"), shared: true }, node("a", "a.txt")];
    backend({ "GET /api/v1/nodes/root/children": () => Response.json({ items, nextCursor: null }) });
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(2));
    expect(within(rows()[0]).getByRole("img", { name: "Открыт доступ другим" })).toBeInTheDocument();
    expect(within(rows()[1]).queryByRole("img", { name: "Открыт доступ другим" })).not.toBeInTheDocument();

    const panel = () => screen.getByRole("complementary", { name: "Сведения" });
    fireEvent.click(rows()[0]);
    expect(within(panel()).getByText("Открыт другим")).toBeInTheDocument();
    fireEvent.click(rows()[1]);
    expect(within(panel()).queryByText("Открыт другим")).not.toBeInTheDocument();
  });
});

describe("FilesPage details panel", () => {
  // jsdom's matchMedia matches nothing: here the window is wide.
  const wide = (matches: boolean) =>
    vi.stubGlobal("matchMedia", (query: string) => ({
      matches: matches && query === "(min-width: 900px)",
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }));
  const panel = () => screen.getByRole("complementary", { name: "Сведения" });

  it("shows the selection's details, and hides on the toggle", async () => {
    wide(true);
    backend({
      "GET /api/v1/nodes/a/content": () => new Response("hello from a"),
      "GET /api/v1/nodes/a/versions": () => Response.json({ items: [], currentAuthor: { id: "u2", displayName: "Борис" } }),
    });
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    expect(screen.queryByRole("complementary", { name: "Сведения" })).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Панель" }));
    expect(localStorage.getItem("cloud.files.panel")).toBe("1");
    expect(within(panel()).getByText("Выберите файл, чтобы увидеть его здесь")).toBeInTheDocument();

    fireEvent.click(rows()[1]);
    expect(await within(panel()).findByText("hello from a")).toBeInTheDocument();
    expect(within(panel()).getByText("a.txt")).toBeInTheDocument();
    expect(within(panel()).getByText("Текстовый файл TXT")).toBeInTheDocument();
    expect(await within(panel()).findByText("Борис")).toBeInTheDocument();
    expect(within(panel()).getByRole("button", { name: "Скачать" })).toBeInTheDocument();
    expect(within(panel()).getByRole("button", { name: "Версии" })).toBeInTheDocument();
    // Open does what a double click does: text goes to the viewer.
    await userEvent.click(within(panel()).getByRole("button", { name: "Открыть" }));
    expect(await screen.findByRole("dialog", { name: "a.txt" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Закрыть" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    fireEvent.click(rows()[2], { ctrlKey: true });
    fireEvent.click(rows()[3], { ctrlKey: true });
    expect(within(panel()).getByText("Выбрано: 3")).toBeInTheDocument();
    expect(within(panel()).getByText("Общий размер: 3 Б")).toBeInTheDocument();

    // A folder: no file actions.
    fireEvent.click(rows()[0]);
    expect(within(panel()).getByText("Папка")).toBeInTheDocument();
    expect(within(panel()).queryByRole("button", { name: "Скачать" })).not.toBeInTheDocument();
    expect(within(panel()).getByRole("button", { name: "Открыть" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Панель" }));
    expect(screen.queryByRole("complementary", { name: "Сведения" })).not.toBeInTheDocument();
    expect(localStorage.getItem("cloud.files.panel")).toBe("0");
  });

  it("stays hidden in a narrow window, even when turned on", async () => {
    wide(false);
    localStorage.setItem("cloud.files.panel", "1");
    backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    expect(screen.queryByRole("complementary", { name: "Сведения" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Панель" })).not.toBeInTheDocument();
  });
});

describe("FilesPage on a phone", () => {
  // A tap: what a finger sends, down then the click the browser makes of it.
  const tap = (el: HTMLElement) => {
    fireEvent.pointerDown(el, { pointerType: "touch", clientX: 10, clientY: 10 });
    fireEvent.pointerUp(el, { pointerType: "touch", clientX: 10, clientY: 10 });
    fireEvent.click(el);
  };
  const longPress = async (el: HTMLElement) => {
    fireEvent.pointerDown(el, { pointerType: "touch", clientX: 10, clientY: 10 });
    await new Promise((r) => setTimeout(r, 600));
    fireEvent.pointerUp(el, { pointerType: "touch", clientX: 10, clientY: 10 });
    fireEvent.click(el);
  };
  // The toolbar's line: what's around its More button.
  const toolbar = () => {
    const line = screen.getByRole("button", { name: "Ещё" }).closest(".mantine-Group-root")?.parentElement;
    if (!line) throw new Error("no toolbar");
    return within(line)
      .getAllByRole("button")
      .map((b) => b.getAttribute("aria-label") ?? b.textContent);
  };

  it("keeps the toolbar to Upload and More, with the rest in More", async () => {
    stubMedia(PHONE_MEDIA);
    backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    // The path's only crumb is text; the list's header sorts by name only.
    expect(toolbar()).toEqual(["Загрузить", "Ещё"]);
    expect(screen.getAllByRole("button", { name: /Сортировать по столбцу/ })).toHaveLength(1);
    // Size and date are under the name.
    expect(rows()[1]).toHaveTextContent("a.txt1 Б ·");

    await userEvent.click(screen.getByRole("button", { name: "Ещё" }));
    expect(await menuItems()).toEqual(["Скачать папку", "Новая папка", "Список", "Плитка", "Имя", "Изменён", "Размер", "По возрастанию", "По убыванию"]);
    fireEvent.click(await menuItem("Новая папка"));
    // Dialogs take the whole screen.
    expect(await screen.findByRole("dialog", { name: "Новая папка" })).toHaveAttribute("data-full-screen");
  });

  it("shows the selection's key actions as icons, and the rest in More", async () => {
    stubMedia(PHONE_MEDIA);
    backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    fireEvent.click(within(rows()[1]).getByRole("checkbox"));
    expect(screen.getByText("Выбрано: 1")).toBeInTheDocument();
    expect(toolbar()).toEqual(["Снять выделение", "Скачать", "Поделиться", "Удалить", "Ещё"]);
    await userEvent.click(screen.getByRole("button", { name: "Ещё" }));
    expect(await menuItems()).toEqual(["Версии", "Переименовать", "Копировать", "Переместить"]);

    await userEvent.click(screen.getByRole("button", { name: "Снять выделение" }));
    expect(selectedIds()).toEqual([]);
    expect(screen.getByRole("button", { name: "Загрузить" })).toBeInTheDocument();
  });

  it("opens with a tap, selects with a long press, then toggles with taps", async () => {
    stubMedia(PHONE_MEDIA, COARSE_MEDIA);
    backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));

    await longPress(rows()[1]);
    expect(selectedIds()).toEqual(["node-a"]);
    // Selecting: taps toggle, and open nothing.
    tap(rows()[2]);
    expect(selectedIds()).toEqual(["node-a", "node-b"]);
    tap(rows()[1]);
    tap(rows()[2]);
    expect(selectedIds()).toEqual([]);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

    // Nothing selected, a tap opens: the folder here.
    tap(rows()[0]);
    expect(await screen.findByText("inside.txt")).toBeInTheDocument();
  });

  it("keeps a mouse's click selecting, even in a phone-sized window", async () => {
    stubMedia(PHONE_MEDIA);
    backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    fireEvent.pointerDown(rows()[1], { pointerType: "mouse" });
    fireEvent.click(rows()[1]);
    expect(selectedIds()).toEqual(["node-a"]);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("opens the context menu from a row's button on a touch screen", async () => {
    stubMedia(PHONE_MEDIA, COARSE_MEDIA);
    backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    fireEvent.click(screen.getByRole("button", { name: "Действия с «a.txt»" }));
    expect(selectedIds()).toEqual(["node-a"]);
    expect(await menuItems()).toEqual(["Скачать", "Версии", "Поделиться", "Переименовать", "Копировать", "Переместить", "Удалить"]);
  });

  it("has no row buttons for a mouse", async () => {
    stubMedia(PHONE_MEDIA);
    backend();
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(4));
    expect(screen.queryByRole("button", { name: "Действия с «a.txt»" })).not.toBeInTheDocument();
  });
});
