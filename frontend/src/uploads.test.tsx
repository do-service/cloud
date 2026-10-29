import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DetailedError, type HttpResponse, type UploadOptions } from "tus-js-client";
import { mockBackend, renderApp } from "./testing";

// Every Upload the app creates, driven by the test: progress(), succeed(),
// fail() stand in for the network.
type FakeUpload = {
  file: File;
  options: UploadOptions;
  started: boolean;
  aborted: boolean | null;
  progress: (sent: number) => void;
  succeed: () => void;
  fail: (status: number, body: string) => void;
};
const uploads: FakeUpload[] = [];

vi.mock("tus-js-client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("tus-js-client")>();
  class Upload {
    fake: FakeUpload;
    constructor(file: File, options: UploadOptions) {
      this.fake = {
        file,
        options,
        started: false,
        aborted: null,
        progress: (sent) => options.onProgress?.(sent, file.size),
        succeed: () => options.onSuccess?.({ lastResponse: response(204, "") }),
        fail: (status, body) => {
          // The typings declare no constructor arguments; the field is public.
          const err = new actual.DetailedError("failed");
          err.originalResponse = response(status, body);
          options.onError?.(err);
        },
      };
      uploads.push(this.fake);
    }
    findPreviousUploads() {
      return Promise.resolve([]);
    }
    resumeFromPreviousUpload() {}
    start() {
      this.fake.started = true;
    }
    abort(terminate: boolean) {
      this.fake.aborted = terminate;
      return Promise.resolve();
    }
  }
  return { ...actual, Upload };
});

// A tus error as tus-js-client hands it to onShouldRetry.
function conflictError(body: string): DetailedError {
  const err = new DetailedError("failed");
  err.originalResponse = response(409, body);
  return err;
}

function response(status: number, body: string): HttpResponse {
  return { getStatus: () => status, getHeader: () => undefined, getBody: () => body, getUnderlyingObject: () => null };
}

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
  uploads.length = 0;
});

const mtime = "2026-10-24T10:00:00Z";
// The owner's rights: everything.
const ALL = ["read", "write", "reshare"];
const node = (id: string, name: string, kind: "dir" | "file" = "file") => ({ id, name, kind, size: 1, etag: "e", mtime, permissions: ALL });
const root = { ...node("r", "", "dir"), path: [{ id: "r", name: "" }] };

function backend(extra: Record<string, () => Response> = {}) {
  return mockBackend({
    "GET /api/v1/auth/me": () => Response.json({ id: "u1", username: "alice", displayName: "Алиса", isAdmin: false }),
    "GET /api/v1/nodes/root": () => Response.json(root),
    "GET /api/v1/nodes/root/children": () => Response.json({ items: [node("a", "a.txt")], nextCursor: null }),
    ...extra,
  });
}

function file(name: string, content = "x", path?: string): File {
  const f = new File([content], name);
  if (path) Object.defineProperty(f, "webkitRelativePath", { value: path });
  return f;
}

const panel = () => screen.getByRole("region", { name: "Загрузки" });

describe("uploads", () => {
  it("uploads picked files into the folder, a few at a time, and refreshes it", async () => {
    const calls = backend();
    renderApp("/files");
    await screen.findByText("a.txt");
    await userEvent.upload(screen.getByTestId("upload-files"), [1, 2, 3, 4, 5].map((i) => file(`f${i}.txt`, "hello")));

    await waitFor(() => expect(uploads.filter((u) => u.started)).toHaveLength(3));
    // A taken name is asked about first, never overwritten silently.
    expect(uploads[0].options.metadata).toEqual({ filename: "f1.txt", parentId: "r", onConflict: "fail" });
    expect(uploads[0].options.endpoint).toBe("/api/v1/uploads/");
    expect(within(panel()).getAllByText("В очереди")).toHaveLength(2);

    uploads[0].progress(2);
    expect(await within(panel()).findByText("2 Б / 5 Б")).toBeInTheDocument();
    const lists = () => calls.filter((c) => c.url.pathname === "/api/v1/nodes/root/children").length;
    const before = lists();
    uploads[0].succeed();
    // A slot frees up and the next file starts; the listing is refetched.
    await waitFor(() => expect(uploads.filter((u) => u.started)).toHaveLength(4));
    await waitFor(() => expect(lists()).toBeGreaterThan(before));
    expect(within(panel()).getByText("Загружено")).toBeInTheDocument();
  });

  it("recreates a picked folder's structure, reusing folders that exist", async () => {
    const calls = backend({
      // Photos exists already: its id is looked up instead.
      "POST /api/v1/nodes/r/children": () => Response.json({ code: "name_conflict", message: "taken" }, { status: 409 }),
      "GET /api/v1/nodes/r/children": () => Response.json({ items: [node("p", "photos", "dir"), node("a", "a.txt")], nextCursor: null }),
      "POST /api/v1/nodes/p/children": () => Response.json(node("y", "2026", "dir"), { status: 201 }),
    });
    renderApp("/files");
    await screen.findByText("a.txt");
    await userEvent.upload(screen.getByTestId("upload-folder"), [
      file("a.jpg", "1", "Photos/2026/a.jpg"),
      file("b.jpg", "2", "Photos/2026/b.jpg"),
      file("c.jpg", "3", "Photos/c.jpg"),
    ]);
    await waitFor(() => expect(uploads.filter((u) => u.started)).toHaveLength(3));
    const parents = Object.fromEntries(uploads.map((u) => [u.file.name, u.options.metadata?.parentId]));
    expect(parents).toEqual({ "a.jpg": "y", "b.jpg": "y", "c.jpg": "p" });
    // One mkdir per folder, however many files are in it.
    expect(calls.filter((c) => c.method === "POST" && c.url.pathname === "/api/v1/nodes/p/children")).toHaveLength(1);
  });

  it("shows why an upload failed and retries it", async () => {
    backend();
    renderApp("/files");
    await screen.findByText("a.txt");
    await userEvent.upload(screen.getByTestId("upload-files"), file("big.iso"));
    await waitFor(() => expect(uploads).toHaveLength(1));
    uploads[0].fail(413, JSON.stringify({ code: "quota_exceeded", message: "no room" }));
    expect(await within(panel()).findByText(/Не хватает места/)).toBeInTheDocument();
    await userEvent.click(within(panel()).getByRole("button", { name: "Повторить загрузку «big.iso»" }));
    await waitFor(() => expect(uploads).toHaveLength(2));
    uploads[1].succeed();
    expect(await within(panel()).findByText("Загружено")).toBeInTheDocument();
  });

  it("asks what to do about taken names, one by one or for all", async () => {
    backend();
    renderApp("/files");
    await screen.findByText("a.txt");
    await userEvent.upload(screen.getByTestId("upload-files"), [file("a.txt"), file("b.txt"), file("c.txt")]);
    await waitFor(() => expect(uploads).toHaveLength(3));
    const conflict = JSON.stringify({ code: "name_conflict", message: "taken" });
    for (const u of uploads) u.fail(409, conflict);
    expect(await within(panel()).findByText("Уже есть файлы с такими именами: 3")).toBeInTheDocument();

    await userEvent.click(within(panel()).getByRole("button", { name: "Заменить «a.txt» новой версией" }));
    await waitFor(() => expect(uploads).toHaveLength(4));
    expect(uploads[3].options.metadata?.onConflict).toBe("replace");

    await userEvent.click(within(panel()).getByRole("button", { name: "Сохранить все" }));
    await waitFor(() => expect(uploads).toHaveLength(6));
    expect(uploads.slice(4).map((u) => u.options.metadata?.onConflict)).toEqual(["rename", "rename"]);
    // A taken name isn't retried on its own: only the user's answers made
    // new uploads.
    expect(uploads[0].options.onShouldRetry?.(conflictError(conflict), 0, uploads[0].options)).toBe(false);
  });

  it("cancels an upload, deleting it on the server", async () => {
    backend();
    renderApp("/files");
    await screen.findByText("a.txt");
    await userEvent.upload(screen.getByTestId("upload-files"), file("a.bin"));
    await waitFor(() => expect(uploads).toHaveLength(1));
    await userEvent.click(within(panel()).getByRole("button", { name: "Отменить загрузку «a.bin»" }));
    expect(uploads[0].aborted).toBe(true);
    expect(within(panel()).getByText("Отменено")).toBeInTheDocument();
    await userEvent.click(within(panel()).getByRole("button", { name: "Очистить" }));
    expect(screen.queryByRole("region", { name: "Загрузки" })).not.toBeInTheDocument();
  });

  it("accepts a dropped folder, walking into it batch by batch", async () => {
    backend({
      "POST /api/v1/nodes/r/children": () => Response.json(node("p", "Photos", "dir"), { status: 201 }),
      "POST /api/v1/nodes/p/children": () => Response.json(node("y", "2026", "dir"), { status: 201 }),
    });
    renderApp("/files");
    await screen.findByText("a.txt");
    const f = (name: string) => ({ isFile: true, isDirectory: false, name, file: (ok: (x: File) => void) => ok(new File(["x"], name)) });
    const dir = (name: string, children: unknown[]) => {
      // Hands out the children two at a time, then an empty batch, as
      // browsers do for big folders.
      const batches = [children.slice(0, 2), children.slice(2), []];
      return { isFile: false, isDirectory: true, name, createReader: () => ({ readEntries: (ok: (b: unknown[]) => void) => ok(batches.shift() ?? []) }) };
    };
    const tree = dir("Photos", [f("1.jpg"), f("2.jpg"), dir("2026", [f("3.jpg")])]);
    const dataTransfer = { types: ["Files"], items: [{ kind: "file", webkitGetAsEntry: () => tree }, { kind: "string", webkitGetAsEntry: () => null }] };
    const target = screen.getByRole("listbox");
    fireEvent.dragEnter(target, { dataTransfer });
    fireEvent.drop(target, { dataTransfer });
    await waitFor(() => expect(uploads.filter((u) => u.started)).toHaveLength(3));
    const parents = Object.fromEntries(uploads.map((u) => [u.file.name, u.options.metadata?.parentId]));
    expect(parents).toEqual({ "1.jpg": "p", "2.jpg": "p", "3.jpg": "y" });
  });

  it("accepts files dropped on the folder", async () => {
    backend();
    renderApp("/files");
    await screen.findByText("a.txt");
    const target = screen.getByRole("listbox");
    const dropped = file("dropped.txt");
    const entry = { isFile: true, isDirectory: false, name: "dropped.txt", file: (ok: (f: File) => void) => ok(dropped) };
    const dataTransfer = { types: ["Files"], items: [{ kind: "file", webkitGetAsEntry: () => entry }] };
    fireEvent.dragEnter(target, { dataTransfer });
    expect(await screen.findByText("Отпустите, чтобы загрузить сюда")).toBeInTheDocument();
    fireEvent.drop(target, { dataTransfer });
    await waitFor(() => expect(uploads.map((u) => u.file.name)).toEqual(["dropped.txt"]));
    expect(screen.queryByText("Отпустите, чтобы загрузить сюда")).not.toBeInTheDocument();
  });
});
