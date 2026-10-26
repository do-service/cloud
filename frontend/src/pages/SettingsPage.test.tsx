import { afterEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { mockBackend, renderApp } from "../testing";

afterEach(() => {
  vi.unstubAllGlobals();
});

const me = () => Response.json({ id: "u1", username: "alice", displayName: "Алиса", isAdmin: false });
const created = "2026-10-24T10:00:00Z";

describe("SettingsPage", () => {
  it("shows the quota and how to connect a drive", async () => {
    mockBackend({
      "GET /api/v1/auth/me": me,
      "GET /api/v1/me/quota": () => Response.json({ usedBytes: 1024 * 1024 * 950, quotaBytes: 1024 * 1024 * 1024 }),
      "GET /api/v1/me/app-passwords": () => Response.json([]),
    });
    renderApp("/settings");
    expect(await screen.findByText("Занято 950 МБ из 1 ГБ")).toBeInTheDocument();
    expect(screen.getByDisplayValue(`${window.location.origin}/dav/files/alice/`)).toBeInTheDocument();
    expect(screen.getByText("Паролей приложений пока нет.")).toBeInTheDocument();
  });

  it("says when there is no limit", async () => {
    mockBackend({
      "GET /api/v1/auth/me": me,
      "GET /api/v1/me/quota": () => Response.json({ usedBytes: 0, quotaBytes: null }),
      "GET /api/v1/me/app-passwords": () => Response.json([]),
    });
    renderApp("/settings");
    expect(await screen.findByText("Занято 0 Б, без ограничения")).toBeInTheDocument();
  });

  it("creates an app password, shows it once, and deletes one", async () => {
    let list: unknown[] = [{ id: "p1", name: "Ноутбук", createdAt: created, lastUsedAt: null }];
    const calls = mockBackend({
      "GET /api/v1/auth/me": me,
      "GET /api/v1/me/quota": () => Response.json({ usedBytes: 0, quotaBytes: null }),
      "GET /api/v1/me/app-passwords": () => Response.json(list),
      "POST /api/v1/me/app-passwords": () => {
        const p = { id: "p2", name: "Телефон", createdAt: created, lastUsedAt: null };
        list = [...list, p];
        return Response.json({ ...p, password: "abcde-fghij-klmno-pqrst-uvwxy" }, { status: 201 });
      },
      "DELETE /api/v1/me/app-passwords/p1": () => {
        list = list.slice(1);
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/settings");
    expect(await screen.findByText("Не использовался")).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText("Название"), "  Телефон ");
    await userEvent.click(screen.getByRole("button", { name: "Создать пароль" }));
    const dialog = await screen.findByRole("dialog", { name: "Пароль «Телефон» создан" });
    expect(within(dialog).getByDisplayValue("abcde-fghij-klmno-pqrst-uvwxy")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST")?.body).toEqual({ name: "Телефон" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Готово" }));
    await waitFor(() => expect(screen.queryByDisplayValue("abcde-fghij-klmno-pqrst-uvwxy")).not.toBeInTheDocument());
    expect(await screen.findByRole("cell", { name: "Телефон" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Удалить пароль «Ноутбук»" }));
    const confirm = await screen.findByRole("dialog", { name: "Удалить пароль?" });
    await userEvent.click(within(confirm).getByRole("button", { name: "Удалить" }));
    await waitFor(() => expect(screen.queryByRole("cell", { name: "Ноутбук" })).not.toBeInTheDocument());
    expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/v1/me/app-passwords/p1")).toBe(true);
  });

  it("lists the signed-in devices and revokes one after confirming", async () => {
    let list: unknown[] = [
      { id: "d1", name: "MacBook Алисы", platform: "macos", clientVersion: "1.2.0", createdAt: created, lastUsedAt: created, lastIp: "192.0.2.7" },
      { id: "d2", name: "Рабочий ПК", platform: "windows", clientVersion: "", createdAt: created, lastUsedAt: null, lastIp: null },
    ];
    const calls = mockBackend({
      "GET /api/v1/auth/me": me,
      "GET /api/v1/me/quota": () => Response.json({ usedBytes: 0, quotaBytes: null }),
      "GET /api/v1/me/app-passwords": () => Response.json([]),
      "GET /api/v1/me/devices": () => Response.json(list),
      "DELETE /api/v1/me/devices/d1": () => {
        list = list.slice(1);
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/settings");
    expect(await screen.findByText("MacBook Алисы")).toBeInTheDocument();
    expect(screen.getByText("macOS · версия 1.2.0")).toBeInTheDocument();
    expect(screen.getByText("192.0.2.7")).toBeInTheDocument();
    expect(screen.getByRole("row", { name: /Рабочий ПК/ })).toHaveTextContent("Windows");
    expect(screen.getByText("Не использовалось")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Отключить устройство «MacBook Алисы»" }));
    const confirm = await screen.findByRole("dialog", { name: "Отключить устройство?" });
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    await userEvent.click(within(confirm).getByRole("button", { name: "Отключить" }));
    await waitFor(() => expect(screen.queryByText("MacBook Алисы")).not.toBeInTheDocument());
    expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/v1/me/devices/d1")).toBe(true);
    expect(screen.getByText("Рабочий ПК")).toBeInTheDocument();
  });

  it("says when no device is signed in", async () => {
    mockBackend({
      "GET /api/v1/auth/me": me,
      "GET /api/v1/me/quota": () => Response.json({ usedBytes: 0, quotaBytes: null }),
      "GET /api/v1/me/app-passwords": () => Response.json([]),
      "GET /api/v1/me/devices": () => Response.json([]),
    });
    renderApp("/settings");
    expect(await screen.findByText("Подключённых устройств нет.")).toBeInTheDocument();
  });

  it("is reached from the user menu", async () => {
    mockBackend({
      "GET /api/v1/auth/me": me,
      "GET /api/v1/nodes/root": () => Response.json({ id: "r", name: "", kind: "dir", size: 0, etag: "e", mtime: created, permissions: ["read", "write", "reshare"], path: [{ id: "r", name: "" }] }),
      "GET /api/v1/nodes/root/children": () => Response.json({ items: [], nextCursor: null }),
      "GET /api/v1/me/quota": () => Response.json({ usedBytes: 0, quotaBytes: null }),
      "GET /api/v1/me/app-passwords": () => Response.json([]),
    });
    renderApp("/files");
    await userEvent.click(await screen.findByRole("button", { name: "Меню пользователя" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Настройки" }));
    expect(await screen.findByRole("heading", { name: "Настройки" })).toBeInTheDocument();
  });
});
