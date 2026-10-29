import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { mockBackend, openSelectionActions, renderApp } from "../testing";

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
});

const mtime = "2026-10-24T10:00:00Z";
const ALL = ["read", "write", "reshare"];
const node = (id: string, name: string, kind: "dir" | "file") => ({ id, name, kind, size: 1, etag: "e", mtime, permissions: ALL });
const editorUrl = "http://localhost:9980/browser/x/cool.html?WOPISrc=http%3A%2F%2Fcloud%3A8080%2Fwopi%2Ffiles%2Fd1&lang=ru";
const backend = {
  "GET /api/v1/auth/me": () => Response.json({ id: "u1", username: "alice", displayName: "Алиса", isAdmin: false }),
  "GET /api/v1/editor": () => Response.json({ extensions: { docx: "edit", xlsx: "edit", txt: "edit", pdf: "view" } }),
  "GET /api/v1/nodes/root": () => Response.json({ ...node("r", "", "dir"), path: [{ id: "r", name: "" }] }),
  "GET /api/v1/nodes/root/children": () =>
    Response.json({ items: [node("d1", "Отчёт.docx", "file"), node("t1", "заметки.txt", "file")], nextCursor: null }),
  "GET /api/v1/nodes/d1": () => Response.json({ ...node("d1", "Отчёт.docx", "file"), path: [{ id: "r", name: "" }, { id: "d1", name: "Отчёт.docx" }] }),
  "POST /api/v1/nodes/d1/edit": () => Response.json({ url: editorUrl, accessToken: "tok-1", accessTokenTtl: 1792891982386 }),
};
const rows = () => within(screen.getByRole("listbox")).getAllByRole("option");

describe("EditorPage", () => {
  it("posts the token to the editor's frame and closes on its word", async () => {
    const close = vi.spyOn(window, "close").mockImplementation(() => undefined);
    const submitted: HTMLFormElement[] = [];
    vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(function (this: HTMLFormElement) {
      submitted.push(this);
    });
    mockBackend(backend);
    renderApp("/edit/d1");
    await waitFor(() => expect(submitted).toHaveLength(1));
    const form = submitted[0];
    // In the form body, not the URL: the token stays out of logs and history.
    expect(form.action).toBe(`${editorUrl}&closebutton=1`);
    expect(form.method).toBe("post");
    expect(form.target).toBe("editor-frame");
    expect(new FormData(form).get("access_token")).toBe("tok-1");
    expect(new FormData(form).get("access_token_ttl")).toBe("1792891982386");
    expect(screen.getByTitle("Редактор: Отчёт.docx")).toHaveAttribute("name", "editor-frame");
    // The whole window: no app header, the tab named after the document.
    expect(screen.queryByRole("banner")).not.toBeInTheDocument();
    expect(document.title).toBe("Отчёт.docx");

    // Only the editor's own origin is listened to.
    act(() => {
      window.dispatchEvent(new MessageEvent("message", { origin: "http://evil.example", data: JSON.stringify({ MessageId: "UI_Close" }) }));
    });
    expect(screen.getByTitle("Редактор: Отчёт.docx")).toBeInTheDocument();
    expect(close).not.toHaveBeenCalled();
    // The tab tries to close; one the browser keeps open goes to the folder.
    act(() => {
      window.dispatchEvent(new MessageEvent("message", { origin: "http://localhost:9980", data: JSON.stringify({ MessageId: "UI_Close" }) }));
    });
    expect(close).toHaveBeenCalledOnce();
    await waitFor(() => expect(rows()).toHaveLength(2));
    expect(screen.getByRole("banner")).toBeInTheDocument();
    expect(document.title).not.toBe("Отчёт.docx");
  });

  it("stays put when its tab does close", async () => {
    vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => undefined);
    const close = vi.spyOn(window, "close").mockImplementation(() => {
      vi.stubGlobal("closed", true);
    });
    mockBackend(backend);
    renderApp("/edit/d1");
    await screen.findByTitle("Редактор: Отчёт.docx");
    act(() => {
      window.dispatchEvent(new MessageEvent("message", { origin: "http://localhost:9980", data: JSON.stringify({ MessageId: "UI_Close" }) }));
    });
    expect(close).toHaveBeenCalledOnce();
    expect(screen.getByTitle("Редактор: Отчёт.docx")).toBeInTheDocument();
  });

  it("says when the editor is unavailable", async () => {
    mockBackend({
      ...backend,
      "POST /api/v1/nodes/d1/edit": () => Response.json({ code: "editor_unavailable", message: "no" }, { status: 503 }),
    });
    const close = vi.spyOn(window, "close").mockImplementation(() => undefined);
    renderApp("/edit/d1");
    expect(await screen.findByText(/Онлайн-редактор сейчас недоступен/)).toBeInTheDocument();
    expect(screen.queryByRole("banner")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Закрыть" }));
    expect(close).toHaveBeenCalledOnce();
    await waitFor(() => expect(rows()).toHaveLength(2));
  });
});

describe("the file manager and the editor", () => {
  it("opens documents in the editor's own tab, text in the viewer and downloads the rest", async () => {
    const opened = vi.spyOn(window, "open").mockImplementation(() => null);
    const clicked: string[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push(new URL(this.href).pathname);
    });
    mockBackend({
      ...backend,
      "GET /api/v1/nodes/root/children": () =>
        Response.json({ items: [node("d1", "Отчёт.docx", "file"), node("t1", "заметки.txt", "file"), node("z1", "архив.zip", "file")], nextCursor: null }),
      "GET /api/v1/nodes/t1/content": () => new Response("купить хлеб"),
    });
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(3));
    // Plain text the editor could open, but the viewer shows it.
    fireEvent.doubleClick(rows()[1]);
    const viewer = await screen.findByRole("dialog", { name: "заметки.txt" });
    expect(await within(viewer).findByText("купить хлеб")).toBeInTheDocument();
    expect(opened).not.toHaveBeenCalled();
    await userEvent.click(within(viewer).getByRole("button", { name: "Закрыть" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    // What nothing shows is downloaded.
    fireEvent.doubleClick(rows()[2]);
    expect(clicked).toEqual(["/api/v1/nodes/z1/content"]);
    fireEvent.click(rows()[0]);
    const editItem = async () => {
      await openSelectionActions();
      return screen.findByRole("menuitem", { name: "Редактировать" });
    };
    const link = await editItem();
    expect(link).toHaveAttribute("href", "/edit/d1");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noopener");
    // A plain click opens the tab by script, so that the editor may close
    // it; a modified one is left to the link.
    expect(fireEvent.click(link)).toBe(false);
    expect(opened).toHaveBeenCalledTimes(1);
    expect(opened).toHaveBeenLastCalledWith("/edit/d1", "_blank", "noopener");
    // Still mounted: Ctrl+click the same item.
    expect(link).toBeInTheDocument();
    expect(fireEvent.click(link, { ctrlKey: true })).toBe(true);
    expect(opened).toHaveBeenCalledTimes(1);
    fireEvent.doubleClick(rows()[0]);
    expect(opened).toHaveBeenCalledTimes(2);
    expect(opened).toHaveBeenLastCalledWith("/edit/d1", "_blank", "noopener");
    // This tab stays in the folder.
    expect(rows()).toHaveLength(3);
  });

  it("refetches the folder when its tab shows again", async () => {
    const calls = mockBackend(backend);
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(2));
    const count = (path: string) => calls.filter((c) => c.url.pathname === path).length;
    const listings = count("/api/v1/nodes/root/children");
    const nodes = count("/api/v1/nodes/root");
    let state: DocumentVisibilityState = "hidden";
    vi.spyOn(document, "visibilityState", "get").mockImplementation(() => state);
    act(() => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(count("/api/v1/nodes/root/children")).toBe(listings);
    state = "visible";
    act(() => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await waitFor(() => expect(count("/api/v1/nodes/root/children")).toBe(listings + 1));
    expect(count("/api/v1/nodes/root")).toBe(nodes + 1);
    // Nothing else: the editor list, "me" and the rest stay cached.
    expect(count("/api/v1/editor")).toBe(1);
  });

  it("creates a new document and opens it", async () => {
    vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => undefined);
    const calls = mockBackend({
      ...backend,
      "POST /api/v1/nodes/r/documents": () => Response.json({ id: "d1", name: "Новая таблица.xlsx" }, { status: 201 }),
    });
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(2));
    await userEvent.click(screen.getByRole("button", { name: "Создать" }));
    const sheet = await screen.findByRole("menuitem", { name: "Новая таблица" });
    // Only what the editor makes: no presentations without pptx.
    expect(screen.queryByRole("menuitem", { name: "Новая презентация" })).not.toBeInTheDocument();
    await userEvent.click(sheet);
    expect(await screen.findByTitle("Редактор: Отчёт.docx")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST" && c.url.pathname === "/api/v1/nodes/r/documents")?.body).toEqual({ name: "Новая таблица.xlsx" });
  });
});
