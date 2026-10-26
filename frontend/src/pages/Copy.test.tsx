import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { jobPoll } from "../jobs";
import { mockBackend, renderApp, selectionAction } from "../testing";

const sizes = { offsetHeight: 600, offsetWidth: 800 };
const saved = Object.fromEntries(Object.keys(sizes).map((k) => [k, Object.getOwnPropertyDescriptor(HTMLElement.prototype, k)]));
beforeAll(() => {
  for (const [k, v] of Object.entries(sizes)) Object.defineProperty(HTMLElement.prototype, k, { configurable: true, value: v });
  jobPoll.intervalMs = 10;
});
afterAll(() => {
  for (const [k, d] of Object.entries(saved)) if (d) Object.defineProperty(HTMLElement.prototype, k, d);
});
afterEach(() => {
  vi.unstubAllGlobals();
});

const mtime = "2026-10-24T10:00:00Z";
const ALL = ["read", "write", "reshare"];
const node = (id: string, name: string, kind: "dir" | "file") => ({ id, name, kind, size: 1, etag: "e", mtime, permissions: ALL });
const base = {
  "GET /api/v1/auth/me": () => Response.json({ id: "u1", username: "alice", displayName: "Алиса", isAdmin: false }),
  "GET /api/v1/editor": () => Response.json({ extensions: {} }),
  "GET /api/v1/nodes/root": () => Response.json({ ...node("r", "", "dir"), path: [{ id: "r", name: "" }] }),
  "GET /api/v1/nodes/r": () => Response.json({ ...node("r", "", "dir"), path: [{ id: "r", name: "" }] }),
  "GET /api/v1/nodes/root/children": () => Response.json({ items: [node("d", "Документы", "dir"), node("f", "a.txt", "file")], nextCursor: null }),
  "GET /api/v1/nodes/r/children": () => Response.json({ items: [node("d", "Документы", "dir"), node("f", "a.txt", "file")], nextCursor: null }),
};
const rows = () => within(screen.getByRole("listbox")).getAllByRole("option");

async function copyHere(name: string) {
  await waitFor(() => expect(rows()).toHaveLength(2));
  fireEvent.click(rows().find((r) => r.textContent?.includes(name)) ?? rows()[0]);
  await selectionAction("Копировать");
  const dialog = await screen.findByRole("dialog", { name: "Куда скопировать (выбрано: 1)" });
  // Into the folder it is in: a copy beside it.
  await userEvent.click(await within(dialog).findByRole("button", { name: "Скопировать сюда" }));
}

describe("copying", () => {
  it("makes a small copy at once", async () => {
    const calls = mockBackend({
      ...base,
      "POST /api/v1/nodes/batch": () => Response.json({ results: [{ id: "f", node: node("f2", "a (2).txt", "file") }] }),
    });
    renderApp("/files");
    await copyHere("a.txt");
    expect(await screen.findByText("Скопировано: 1")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST")?.body).toEqual({ ops: [{ op: "copy", id: "f", parentId: "r" }] });
  });

  it("follows a big copy to its end", async () => {
    let asked = 0;
    mockBackend({
      ...base,
      "POST /api/v1/nodes/batch": () => Response.json({ results: [{ id: "d", job: 7 }] }),
      "GET /api/v1/jobs/7": () => Response.json(++asked < 3 ? { id: 7, state: "running" } : { id: 7, state: "done" }),
    });
    renderApp("/files");
    await copyHere("Документы");
    expect(await screen.findByText(/Копируется «Документы»/)).toBeInTheDocument();
    expect(await screen.findByText("«Документы» скопирован")).toBeInTheDocument();
    expect(asked).toBe(3);
  });

  it("says why a big copy failed", async () => {
    mockBackend({
      ...base,
      "POST /api/v1/nodes/batch": () => Response.json({ results: [{ id: "d", job: 8 }] }),
      "GET /api/v1/jobs/8": () => Response.json({ id: 8, state: "failed", error: "quota_exceeded" }),
    });
    renderApp("/files");
    await copyHere("Документы");
    expect(await screen.findByText(/Не удалось скопировать «Документы»/)).toBeInTheDocument();
  });
});
