import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { mockBackend, renderApp } from "../testing";

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
  vi.restoreAllMocks();
});

const mtime = "2026-10-24T10:00:00Z";
const ALL = ["read", "write", "reshare"];
const file = (id: string, name: string, size = 1000) => ({ id, name, kind: "file", size, etag: `e-${id}`, mtime, permissions: ALL });
const backend = {
  "GET /api/v1/auth/me": () => Response.json({ id: "u1", username: "alice", displayName: "Алиса", isAdmin: false }),
  "GET /api/v1/editor": () => Response.json({ extensions: {} }),
  "GET /api/v1/nodes/root": () => Response.json({ id: "r", name: "", kind: "dir", size: 0, etag: "e", mtime, permissions: ALL, path: [{ id: "r", name: "" }] }),
  "GET /api/v1/nodes/root/children": () =>
    Response.json({
      items: [file("a", "море.jpg"), file("b", "отчёт.pdf"), file("c", "заметки.txt"), file("d", "IMG_0001.HEIC"), file("z", "пустой.png", 0), file("y", "архив.zip")],
      nextCursor: null,
    }),
  "GET /api/v1/nodes/c/content": () => new Response("купить хлеб"),
};
const rows = () => within(screen.getByRole("listbox")).getAllByRole("option");

describe("previews", () => {
  it("shows thumbnails of what has them, the icon otherwise", async () => {
    mockBackend(backend);
    const { container } = renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(6));
    const thumbs = Array.from(container.querySelectorAll<HTMLImageElement>("img[loading=lazy]")).map((i) => new URL(i.src).pathname + new URL(i.src).search);
    // Not the text file, nor an empty one.
    expect(thumbs).toEqual([
      "/api/v1/nodes/a/preview?size=s&v=e-a",
      "/api/v1/nodes/b/preview?size=s&v=e-b",
      "/api/v1/nodes/d/preview?size=s&v=e-d",
    ]);
    // One the server can't make leaves the icon.
    const img = container.querySelector<HTMLImageElement>("img[loading=lazy]");
    if (!img) throw new Error("no thumbnail");
    fireEvent.error(img);
    await waitFor(() => expect(container.querySelectorAll("img[loading=lazy]")).toHaveLength(2));
  });

  it("opens images, PDFs and text in the viewer and steps through them", async () => {
    const clicked: string[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push(new URL(this.href).pathname);
    });
    mockBackend(backend);
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(6));
    fireEvent.doubleClick(rows()[0]);
    let viewer = await screen.findByRole("dialog", { name: "море.jpg" });
    expect(within(viewer).getByRole("img", { name: "море.jpg" })).toHaveAttribute("src", "/api/v1/nodes/a/preview?size=l&v=e-a");
    expect(within(viewer).getByText("1 из 5")).toBeInTheDocument();
    expect(within(viewer).queryByRole("button", { name: "Предыдущий" })).not.toBeInTheDocument();

    // The keyboard steps on: a PDF is shown by the browser, inline.
    await userEvent.keyboard("{ArrowRight}");
    viewer = await screen.findByRole("dialog", { name: "отчёт.pdf" });
    expect(within(viewer).getByTitle("отчёт.pdf")).toHaveAttribute("src", "/api/v1/nodes/b/content?inline=1");
    await userEvent.click(within(viewer).getByRole("button", { name: "Следующий" }));
    viewer = await screen.findByRole("dialog", { name: "заметки.txt" });
    expect(await within(viewer).findByText("купить хлеб")).toBeInTheDocument();
    await userEvent.click(within(viewer).getByRole("button", { name: "Следующий" }));
    viewer = await screen.findByRole("dialog", { name: "IMG_0001.HEIC" });
    await userEvent.click(within(viewer).getByRole("button", { name: "Скачать" }));
    expect(clicked).toEqual(["/api/v1/nodes/d/content"]);
    // An empty file has nothing to read.
    await userEvent.click(within(viewer).getByRole("button", { name: "Следующий" }));
    viewer = await screen.findByRole("dialog", { name: "пустой.png" });
    expect(within(viewer).getByText("Файл пуст")).toBeInTheDocument();
    expect(within(viewer).queryByRole("button", { name: "Следующий" })).not.toBeInTheDocument();
    await userEvent.click(within(viewer).getByRole("button", { name: "Закрыть" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    // What the viewer can't show is downloaded, as before.
    fireEvent.doubleClick(rows()[5]);
    expect(clicked).toEqual(["/api/v1/nodes/d/content", "/api/v1/nodes/y/content"]);
  });
});

describe("text viewer", () => {
  const open = async (content: () => Response, size = 1000) => {
    const ranges: (string | null)[] = [];
    mockBackend({
      ...backend,
      "GET /api/v1/nodes/root/children": () => Response.json({ items: [file("c", "page.html", size)], nextCursor: null }),
      "GET /api/v1/nodes/c/content": (_url, init) => {
        ranges.push(new Headers(init?.headers).get("Range"));
        return content();
      },
    });
    renderApp("/files");
    await waitFor(() => expect(rows()).toHaveLength(1));
    fireEvent.doubleClick(rows()[0]);
    return { viewer: await screen.findByRole("dialog", { name: "page.html" }), ranges };
  };

  it("shows markup as it is, never as HTML", async () => {
    const { viewer, ranges } = await open(() => new Response("<b>x</b>\nline 2"));
    const pre = await within(viewer).findByText(/<b>x<\/b>/);
    expect(pre.tagName).toBe("PRE");
    expect(pre.textContent).toBe("<b>x</b>\nline 2");
    expect(viewer.querySelector("b")).toBeNull();
    // Only the first megabyte is asked for.
    expect(ranges).toEqual(["bytes=0-1048575"]);
    expect(within(viewer).queryByText("Показан первый 1 МБ")).not.toBeInTheDocument();
  });

  it("says when it shows only the start, and offers the whole file", async () => {
    const clicked: string[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push(new URL(this.href).pathname);
    });
    // Cut in the middle of "ж" (two bytes): the half is dropped.
    const bytes = new TextEncoder().encode("начало ж");
    const { viewer } = await open(
      () => new Response(bytes.slice(0, bytes.length - 1), { status: 206, headers: { "Content-Range": `bytes 0-${bytes.length - 2}/5000000` } }),
      5_000_000,
    );
    expect(await within(viewer).findByText("Показан первый 1 МБ")).toBeInTheDocument();
    expect(viewer.querySelector("pre")?.textContent).toBe("начало ");
    const notice = within(viewer).getByText("Показан первый 1 МБ").parentElement;
    if (!notice) throw new Error("no notice");
    await userEvent.click(within(notice).getByRole("button", { name: "Скачать" }));
    expect(clicked).toEqual(["/api/v1/nodes/c/content"]);
  });
});
