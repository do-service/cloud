import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { UploadOptions } from "tus-js-client";
import { mockBackend, renderApp, selectionAction, unauthorized } from "../testing";

// Uploads through a drop link, driven by the test.
const started: { file: File; options: UploadOptions }[] = [];
vi.mock("tus-js-client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("tus-js-client")>();
  class Upload {
    constructor(file: File, options: UploadOptions) {
      started.push({ file, options });
    }
    start() {}
  }
  return { ...actual, Upload };
});

const sizes = { offsetHeight: 600, offsetWidth: 800, clientHeight: 600 };
const saved = Object.fromEntries(Object.keys(sizes).map((k) => [k, Object.getOwnPropertyDescriptor(HTMLElement.prototype, k)]));
beforeAll(() => {
  for (const [k, v] of Object.entries(sizes)) Object.defineProperty(HTMLElement.prototype, k, { configurable: true, value: v });
});
afterAll(() => {
  for (const [k, d] of Object.entries(saved)) if (d) Object.defineProperty(HTMLElement.prototype, k, d);
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  started.length = 0;
});

const T = "/s/tok/api";
const mtime = "2026-10-24T10:00:00Z";
const node = (id: string, name: string, kind: "dir" | "file", mime?: string) => ({ id, name, kind, size: 5, etag: "e", mtime, permissions: ["read"], mime });
const info = (over: Record<string, unknown> = {}) =>
  Response.json({ name: "Проект", kind: "dir", mode: "view", needsPassword: false, unlocked: true, expiresAt: null, rootId: "r", ...over });
const folder = {
  [`GET ${T}/nodes/r`]: () => Response.json({ ...node("r", "Проект", "dir"), path: [{ id: "r", name: "Проект" }] }),
  [`GET ${T}/nodes/r/children`]: () =>
    Response.json({ items: [node("d", "Эскизы", "dir"), node("p", "plan.pdf", "file", "application/pdf"), node("h", "page.html", "file", "text/html")], nextCursor: null }),
  [`GET ${T}/nodes/d`]: () => Response.json({ ...node("d", "Эскизы", "dir"), path: [{ id: "r", name: "Проект" }, { id: "d", name: "Эскизы" }] }),
  [`GET ${T}/nodes/d/children`]: () => Response.json({ items: [], nextCursor: null }),
};
const anonymous = { "GET /api/v1/auth/me": unauthorized };
const rows = () => within(screen.getByRole("listbox")).getAllByRole("option");

describe("PublicLinkPage", () => {
  it("says when a link is gone", async () => {
    mockBackend({ ...anonymous, [`GET ${T}/info`]: () => Response.json({ code: "not_found", message: "no" }, { status: 404 }) });
    renderApp("/s/tok");
    expect(await screen.findByText("Ссылка недоступна")).toBeInTheDocument();
  });

  it("asks for the password, then shows the folder", async () => {
    let unlocked = false;
    const calls = mockBackend({
      ...anonymous,
      ...folder,
      [`GET ${T}/info`]: () => info({ needsPassword: true, unlocked }),
      [`POST ${T}/unlock`]: () => {
        const wrong = calls[calls.length - 1].body;
        if (JSON.stringify(wrong) !== JSON.stringify({ password: "pw" })) return Response.json({ code: "invalid_credentials", message: "no" }, { status: 401 });
        unlocked = true;
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/s/tok");
    await userEvent.type(await screen.findByLabelText(/Пароль/), "nope");
    await userEvent.click(screen.getByRole("button", { name: "Открыть" }));
    expect(await screen.findByText("Неверное имя пользователя или пароль.")).toBeInTheDocument();
    await userEvent.clear(screen.getByLabelText(/Пароль/));
    await userEvent.type(screen.getByLabelText(/Пароль/), "pw");
    await userEvent.click(screen.getByRole("button", { name: "Открыть" }));
    await waitFor(() => expect(rows()).toHaveLength(3));
  });

  it("browses a view link and shows only what the browser can", async () => {
    mockBackend({ ...anonymous, ...folder, [`GET ${T}/info`]: () => info() });
    const opened = vi.spyOn(window, "open").mockImplementation(() => null);
    renderApp("/s/tok");
    await waitFor(() => expect(rows()).toHaveLength(3));
    // No downloading through a view link.
    expect(screen.queryByRole("button", { name: /Скачать/ })).not.toBeInTheDocument();
    // A PDF opens in the viewer, shown inline; a view link offers no download.
    fireEvent.doubleClick(rows()[1]);
    const viewer = await screen.findByRole("dialog", { name: "plan.pdf" });
    expect(within(viewer).getByTitle("plan.pdf")).toHaveAttribute("src", "/s/tok/api/nodes/p/content?inline=1");
    expect(within(viewer).queryByRole("button", { name: "Скачать" })).not.toBeInTheDocument();
    await userEvent.click(within(viewer).getByRole("button", { name: "Закрыть" }));
    expect(opened).not.toHaveBeenCalled();
    fireEvent.doubleClick(rows()[2]);
    expect(await screen.findByText(/нельзя посмотреть в браузере/)).toBeInTheDocument();
    fireEvent.doubleClick(rows()[0]);
    expect(await screen.findByText("Папка пуста")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Проект" })).toHaveAttribute("href", "/s/tok");
  });

  it("downloads a folder as ZIP through a download link", async () => {
    mockBackend({ ...anonymous, ...folder, [`GET ${T}/info`]: () => info({ mode: "download" }) });
    const clicked: string[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push(new URL(this.href).pathname);
    });
    renderApp("/s/tok");
    await waitFor(() => expect(rows()).toHaveLength(3));
    await userEvent.click(screen.getByRole("button", { name: "Скачать всё" }));
    // What the viewer can't show downloads; a PDF opens there, with a download.
    fireEvent.doubleClick(rows()[2]);
    fireEvent.doubleClick(rows()[1]);
    await userEvent.click(within(await screen.findByRole("dialog", { name: "plan.pdf" })).getByRole("button", { name: "Скачать" }));
    expect(clicked).toEqual(["/s/tok/api/nodes/r/archive", "/s/tok/api/nodes/h/content", "/s/tok/api/nodes/p/content"]);
  });

  it("takes files through an upload link, showing nothing of the folder", async () => {
    const calls = mockBackend({ ...anonymous, [`GET ${T}/info`]: () => info({ mode: "upload", name: "Входящие" }) });
    renderApp("/s/tok");
    expect(await screen.findByText("Загрузка в «Входящие»")).toBeInTheDocument();
    await userEvent.upload(screen.getByTestId("drop-input"), [new File(["x"], "скан.pdf")]);
    expect(started.map((u) => [u.file.name, u.options.endpoint])).toEqual([["скан.pdf", "/s/tok/api/uploads/"]]);
    started[0].options.onSuccess?.({ lastResponse: { getStatus: () => 204, getHeader: () => undefined, getBody: () => "", getUnderlyingObject: () => null } });
    expect(await screen.findByText("Загружено")).toBeInTheDocument();
    expect(calls.some((c) => c.url.pathname.includes("/children"))).toBe(false);
  });
});

describe("LinksPanel", () => {
  it("makes a link with a password and expiry, and offers to copy it", async () => {
    let links: unknown[] = [];
    const calls = mockBackend({
      "GET /api/v1/auth/me": () => Response.json({ id: "u1", username: "alice", displayName: "Алиса", isAdmin: false }),
      "GET /api/v1/nodes/root": () => Response.json({ ...node("r", "", "dir"), permissions: ["read", "write", "reshare"], path: [{ id: "r", name: "" }] }),
      "GET /api/v1/nodes/root/children": () =>
        Response.json({ items: [{ ...node("d", "Docs", "dir"), permissions: ["read", "write", "reshare"] }], nextCursor: null }),
      "GET /api/v1/nodes/d/shares": () => Response.json({ items: [] }),
      "GET /api/v1/directory": () => Response.json({ items: [] }),
      "GET /api/v1/nodes/d/links": () => Response.json({ items: links }),
      "POST /api/v1/nodes/d/links": () => {
        links = [{ id: "l1", token: "abc", mode: "download", hasPassword: true, expiresAt: "2026-10-31T10:00:00Z", createdAt: mtime }];
        return Response.json(links[0], { status: 201 });
      },
    });
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(1));
    fireEvent.click(rows()[0]);
    await selectionAction("Поделиться");
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("tab", { name: "Ссылка" }));
    expect(await within(dialog).findByText("Ссылок пока нет.")).toBeInTheDocument();
    // A folder the user can write to may also take uploads.
    await userEvent.click(within(dialog).getByLabelText("Что можно"));
    expect(screen.getByRole("option", { name: "Только загружать файлы", hidden: true })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("option", { name: "Смотреть и скачивать", hidden: true }));
    await userEvent.click(within(dialog).getByLabelText("Срок"));
    await userEvent.click(screen.getByRole("option", { name: "7 дн.", hidden: true }));
    await userEvent.type(within(dialog).getByLabelText(/^Пароль/), "секрет");
    await userEvent.click(within(dialog).getByRole("button", { name: "Создать ссылку" }));

    expect(await within(dialog).findByDisplayValue(`${window.location.origin}/s/abc`)).toBeInTheDocument();
    expect(within(dialog).getByText("С паролем")).toBeInTheDocument();
    const body = calls.find((c) => c.method === "POST" && c.url.pathname === "/api/v1/nodes/d/links")?.body;
    expect(body).toMatchObject({ mode: "download", password: "секрет" });
    if (typeof body !== "object" || body === null || !("expiresAt" in body) || typeof body.expiresAt !== "string") throw new Error("no expiry sent");
    const days = (Date.parse(body.expiresAt) - Date.now()) / 86_400_000;
    expect(days).toBeGreaterThan(6.9);
    expect(days).toBeLessThan(7.1);
  });

  it("lists a link whose address is lost to a key change, and revokes it", async () => {
    let links: unknown[] = [{ id: "l1", token: null, mode: "view", hasPassword: false, expiresAt: null, createdAt: mtime }];
    const calls = mockBackend({
      "GET /api/v1/auth/me": () => Response.json({ id: "u1", username: "alice", displayName: "Алиса", isAdmin: false }),
      "GET /api/v1/nodes/root": () => Response.json({ ...node("r", "", "dir"), permissions: ["read", "write", "reshare"], path: [{ id: "r", name: "" }] }),
      "GET /api/v1/nodes/root/children": () =>
        Response.json({ items: [{ ...node("d", "Docs", "dir"), permissions: ["read", "write", "reshare"] }], nextCursor: null }),
      "GET /api/v1/nodes/d/shares": () => Response.json({ items: [] }),
      "GET /api/v1/directory": () => Response.json({ items: [] }),
      "GET /api/v1/nodes/d/links": () => Response.json({ items: links }),
      "DELETE /api/v1/links/l1": () => {
        links = [];
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(1));
    fireEvent.click(rows()[0]);
    await selectionAction("Поделиться");
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("tab", { name: "Ссылка" }));
    expect(await within(dialog).findByText(/Адрес не показать/)).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "Скопировать" })).not.toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Удалить ссылку" }));
    expect(await within(dialog).findByText("Ссылок пока нет.")).toBeInTheDocument();
    expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/v1/links/l1")).toBe(true);
  });
});
