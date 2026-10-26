import { afterEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PHONE_MEDIA } from "../media";
import { currentUrl, mockBackend, renderApp, stubMedia } from "../testing";

afterEach(() => {
  vi.unstubAllGlobals();
});

const at = "2026-10-24T10:00:00Z";
const root = { id: "a0", username: "root", displayName: "Root", isAdmin: true };
const account = (id: string, username: string, over: Record<string, unknown> = {}) => ({
  id,
  username,
  displayName: username,
  email: "",
  isAdmin: false,
  status: "active",
  quotaBytes: 10 * 1024 ** 3,
  usedBytes: 1024 ** 3,
  hasPassword: true,
  providers: [],
  createdAt: at,
  lastSeenAt: null,
  ...over,
});
const asAdmin = { "GET /api/v1/auth/me": () => Response.json(root) };

describe("AdminPage", () => {
  it("is only for administrators", async () => {
    mockBackend({
      "GET /api/v1/auth/me": () => Response.json({ ...root, isAdmin: false }),
      "GET /api/v1/nodes/root": () => Response.json({ id: "r", name: "", kind: "dir", size: 0, etag: "e", mtime: at, permissions: ["read"], path: [] }),
      "GET /api/v1/nodes/root/children": () => Response.json({ items: [], nextCursor: null }),
    });
    renderApp("/admin");
    expect(await screen.findByText("Этот раздел — только для администраторов.")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Админка" })).not.toBeInTheDocument();
  });

  it("creates, changes and deletes users", async () => {
    let users = [account("a0", "root", { isAdmin: true, displayName: "Root" }), account("a1", "bob", { displayName: "Боб" })];
    const calls = mockBackend({
      ...asAdmin,
      "GET /api/v1/admin/users": () => Response.json({ items: users, nextCursor: null }),
      "POST /api/v1/admin/users": () => {
        users = [...users, account("a2", "carol")];
        return Response.json(users[2], { status: 201 });
      },
      "PATCH /api/v1/admin/users/a1": () => Response.json({ ...users[1], status: "disabled", quotaBytes: null }),
      "DELETE /api/v1/admin/users/a1": () => new Response(null, { status: 202 }),
    });
    renderApp("/admin");
    expect(await screen.findByText("Боб")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Админка" })).toHaveAttribute("href", "/admin");
    expect(screen.getAllByText("1 ГБ из 10 ГБ")).toHaveLength(2);

    await userEvent.click(screen.getByRole("button", { name: "Новый пользователь" }));
    const create = await screen.findByRole("dialog", { name: "Новый пользователь" });
    await userEvent.type(within(create).getByLabelText(/Логин/), "carol");
    await userEvent.type(within(create).getByLabelText(/^Пароль/), "password1");
    await userEvent.click(within(create).getByRole("button", { name: "Создать" }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === "POST")?.body).toEqual({
        username: "carol",
        displayName: "",
        password: "password1",
        isAdmin: false,
        quotaBytes: 10 * 1024 ** 3,
      }),
    );
    expect(await screen.findByRole("button", { name: "Изменить carol" })).toBeInTheDocument();

    // Only what changed is sent.
    await userEvent.click(screen.getByRole("button", { name: "Изменить bob" }));
    const edit = await screen.findByRole("dialog", { name: "Пользователь bob" });
    await userEvent.click(within(edit).getByLabelText("Без ограничения"));
    await userEvent.click(within(edit).getByRole("switch", { name: /^Заблокирован/ }));
    await userEvent.click(within(edit).getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ quotaBytes: null, status: "disabled" }));

    // Deleting asks for the username.
    await userEvent.click(screen.getByRole("button", { name: "Изменить bob" }));
    await userEvent.click(within(await screen.findByRole("dialog", { name: "Пользователь bob" })).getByRole("button", { name: "Удалить пользователя" }));
    const confirm = await screen.findByRole("dialog", { name: "Удалить bob?" });
    const go = within(confirm).getByRole("button", { name: "Удалить пользователя" });
    expect(go).toBeDisabled();
    await userEvent.type(within(confirm).getByLabelText(/Чтобы подтвердить/), "bob");
    await userEvent.click(go);
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/v1/admin/users/a1")).toBe(true));
  });

  it("shows a user's devices and revokes one", async () => {
    let devices: unknown[] = [{ id: "d1", name: "Ноутбук Боба", platform: "linux", clientVersion: "0.9", createdAt: at, lastUsedAt: at, lastIp: "10.0.0.5" }];
    const calls = mockBackend({
      ...asAdmin,
      "GET /api/v1/admin/users": () => Response.json({ items: [account("a1", "bob")], nextCursor: null }),
      "GET /api/v1/admin/users/a1/devices": () => Response.json(devices),
      "DELETE /api/v1/admin/users/a1/devices/d1": () => {
        devices = [];
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/admin");
    await userEvent.click(await screen.findByRole("button", { name: "Изменить bob" }));
    const edit = await screen.findByRole("dialog", { name: "Пользователь bob" });
    expect(await within(edit).findByText("Ноутбук Боба")).toBeInTheDocument();
    expect(within(edit).getByText("Linux · версия 0.9")).toBeInTheDocument();

    await userEvent.click(within(edit).getByRole("button", { name: "Отключить устройство «Ноутбук Боба»" }));
    const confirm = await screen.findByRole("dialog", { name: "Отключить устройство?" });
    await userEvent.click(within(confirm).getByRole("button", { name: "Отключить" }));
    expect(await within(edit).findByText("Подключённых устройств нет.")).toBeInTheDocument();
    expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/v1/admin/users/a1/devices/d1")).toBe(true);
    // Revoking a device saves nothing else of the account.
    expect(calls.some((c) => c.method === "PATCH")).toBe(false);
  });

  it("keeps an admin from locking themselves out", async () => {
    mockBackend({ ...asAdmin, "GET /api/v1/admin/users": () => Response.json({ items: [account("a0", "root", { isAdmin: true })], nextCursor: null }) });
    renderApp("/admin");
    await userEvent.click(await screen.findByRole("button", { name: "Изменить root" }));
    const edit = await screen.findByRole("dialog", { name: "Пользователь root" });
    expect(within(edit).getByRole("switch", { name: /^Администратор/ })).toBeDisabled();
    expect(within(edit).getByRole("switch", { name: /^Заблокирован/ })).toBeDisabled();
    expect(within(edit).getByRole("button", { name: "Удалить пользователя" })).toBeDisabled();
  });

  it("manages groups and their members", async () => {
    let groups = [{ id: "g1", name: "Бухгалтерия", members: [{ type: "user", id: "a1", name: "bob" }] }];
    const calls = mockBackend({
      ...asAdmin,
      "GET /api/v1/admin/groups": () => Response.json({ items: groups }),
      "POST /api/v1/admin/groups": () => {
        groups = [...groups, { id: "g2", name: "Склад", members: [] }];
        return Response.json(groups[1], { status: 201 });
      },
      "GET /api/v1/admin/users": () => Response.json({ items: [account("a1", "bob"), account("a2", "carol")], nextCursor: null }),
      "PUT /api/v1/admin/groups/g1/members/a2": () => new Response(null, { status: 204 }),
      "DELETE /api/v1/admin/groups/g1/members/a1": () => new Response(null, { status: 204 }),
    });
    renderApp("/admin?tab=groups");
    expect(await screen.findByText("Бухгалтерия")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Новая группа"), "Склад");
    await userEvent.click(screen.getByRole("button", { name: "Создать" }));
    expect(await screen.findByText("Склад")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST")?.body).toEqual({ name: "Склад" });

    await userEvent.type(screen.getByRole("combobox", { name: "Добавить участника в группу Бухгалтерия" }), "ca");
    // bob is in already: only carol is offered.
    await userEvent.click(await screen.findByRole("option", { name: "carol (carol)", hidden: true }));
    expect(screen.queryByRole("option", { name: "bob (bob)", hidden: true })).not.toBeInTheDocument();
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    await userEvent.click(screen.getByRole("button", { name: "Убрать bob из группы Бухгалтерия" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname.endsWith("/members/a1"))).toBe(true));
  });

  const entry = (id: number, action: string, target = "") => ({
    id,
    at,
    actor: { id: "a0", name: "root" },
    ip: "192.0.2.1",
    action,
    target: { id: "", name: target },
    details: action === "admin.user.update" ? { status: "disabled" } : {},
  });
  const auditPage = (url: URL, total: number, items = [entry(1, "auth.login")]) =>
    Response.json({ items, total, page: Number(url.searchParams.get("page")), size: Number(url.searchParams.get("size")) });
  const auditCalls = (calls: { url: URL }[]) => calls.filter((c) => c.url.pathname === "/api/v1/admin/audit").map((c) => c.url.searchParams);
  const lastAudit = (calls: { url: URL }[]): URLSearchParams | undefined => {
    const sent = auditCalls(calls);
    return sent[sent.length - 1];
  };

  it("shows the audit log, narrowed by area or action", async () => {
    const calls = mockBackend({
      ...asAdmin,
      "GET /api/v1/admin/audit": (url) => {
        const action = url.searchParams.get("action");
        const items =
          action === "admin." || action === "admin.user.update"
            ? [entry(2, "admin.user.update", "bob")]
            : [entry(2, "admin.user.update", "bob"), entry(1, "auth.login_failed")];
        return auditPage(url, items.length, items);
      },
    });
    renderApp("/admin?tab=audit");
    // The action picker lists the same names: look in the table.
    const table = await screen.findByRole("table");
    expect(within(table).getByText("Неудачный вход")).toBeInTheDocument();
    expect(within(table).getByText("Пользователь изменён")).toBeInTheDocument();
    expect(within(table).getByText("status: disabled")).toBeInTheDocument();
    expect(screen.getByText("Всего 2 записи")).toBeInTheDocument();
    // One page: no page numbers.
    expect(screen.queryByRole("button", { name: "Следующая страница" })).not.toBeInTheDocument();
    expect(lastAudit(calls)?.toString()).toBe("page=1&size=50");

    await userEvent.click(screen.getByRole("combobox", { name: "Действие" }));
    await userEvent.click(await screen.findByRole("option", { name: "Действия администраторов — все", hidden: true }));
    await waitFor(() => expect(within(screen.getByRole("table")).queryByText("Неудачный вход")).not.toBeInTheDocument());
    expect(lastAudit(calls)?.get("action")).toBe("admin.");
    expect(currentUrl()).toBe("/admin?tab=audit&action=admin.");
    expect(screen.getByText("Всего 1 запись")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("combobox", { name: "Действие" }));
    await userEvent.click(await screen.findByRole("option", { name: "Пользователь изменён", hidden: true }));
    await waitFor(() => expect(lastAudit(calls)?.get("action")).toBe("admin.user.update"));
  });

  it("takes the audit page and filters from the URL, ignoring what it can't use", async () => {
    const calls = mockBackend({ ...asAdmin, "GET /api/v1/admin/audit": (url) => auditPage(url, 300) });
    renderApp("/admin?tab=audit&page=2&size=100&action=share.grant&actor=Ivan&from=2026-01-15&to=2026-01-16&q=doc&ip=10.0.0.0/8");
    expect(await screen.findByText("Всего 300 записей")).toBeInTheDocument();
    const sent = lastAudit(calls);
    expect(sent?.get("page")).toBe("2");
    expect(sent?.get("size")).toBe("100");
    expect(sent?.get("action")).toBe("share.grant");
    expect(sent?.get("actor")).toBe("Ivan");
    expect(sent?.get("q")).toBe("doc");
    expect(sent?.get("ip")).toBe("10.0.0.0/8");
    // Local midnights; "по" includes its whole day.
    expect(sent?.get("from")).toMatch(/^2026-01-15T00:00:00[+-]\d\d:\d\d$/);
    expect(sent?.get("to")).toMatch(/^2026-01-17T00:00:00[+-]\d\d:\d\d$/);
    expect(new Date(sent?.get("from") ?? "").getTime()).toBe(new Date(2026, 0, 15).getTime());
    expect(screen.getByRole("combobox", { name: "Пользователь" })).toHaveValue("Ivan");
    expect(screen.getByLabelText("С")).toHaveValue("2026-01-15");
    expect(screen.getByRole("button", { name: "3" })).toBeInTheDocument();
  });

  it("falls back to the first page and defaults on a bad URL", async () => {
    const calls = mockBackend({ ...asAdmin, "GET /api/v1/admin/audit": (url) => auditPage(url, 1) });
    renderApp("/admin?tab=audit&page=-3&size=7&action=nope&from=yesterday");
    expect(await screen.findByText("Всего 1 запись")).toBeInTheDocument();
    expect(lastAudit(calls)?.toString()).toBe("page=1&size=50");
  });

  it("pages through the audit log and changes the page size", async () => {
    const calls = mockBackend({ ...asAdmin, "GET /api/v1/admin/audit": (url) => auditPage(url, 120) });
    renderApp("/admin?tab=audit&actor=root");
    expect(await screen.findByText("Всего 120 записей")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "3" }));
    await waitFor(() => expect(lastAudit(calls)?.get("page")).toBe("3"));
    expect(lastAudit(calls)?.get("actor")).toBe("root");
    expect(currentUrl()).toBe("/admin?tab=audit&actor=root&page=3");

    await userEvent.click(screen.getByRole("combobox", { name: "Показывать" }));
    await userEvent.click(await screen.findByRole("option", { name: "25", hidden: true }));
    await waitFor(() => expect(lastAudit(calls)?.get("size")).toBe("25"));
    // A new size starts over from the first page.
    expect(lastAudit(calls)?.get("page")).toBe("1");
    expect(currentUrl()).toBe("/admin?tab=audit&actor=root&size=25");
    expect(await screen.findByRole("button", { name: "5" })).toBeInTheDocument();
  });

  it("filters the audit log as you type, back to the first page", async () => {
    const calls = mockBackend({
      ...asAdmin,
      "GET /api/v1/admin/audit": (url) => auditPage(url, 120),
      "GET /api/v1/admin/users": () => Response.json({ items: [account("u1", "ivan")], nextCursor: null }),
    });
    renderApp("/admin?tab=audit&page=2");
    expect(await screen.findByText("Всего 120 записей")).toBeInTheDocument();
    await userEvent.type(screen.getByRole("combobox", { name: "Пользователь" }), "iv");
    expect(await screen.findByRole("option", { name: "ivan", hidden: true })).toBeInTheDocument();
    await waitFor(() => expect(lastAudit(calls)?.get("actor")).toBe("iv"));
    expect(lastAudit(calls)?.get("page")).toBe("1");
    // Typing is sent once it pauses, not a letter at a time.
    expect(auditCalls(calls).some((p) => p.get("actor") === "i")).toBe(false);
    await userEvent.type(screen.getByLabelText("Объект"), "report");
    await userEvent.type(screen.getByLabelText("IP"), "10.0.0.1");
    await waitFor(() => expect(currentUrl()).toBe("/admin?tab=audit&actor=iv&q=report&ip=10.0.0.1"));
    expect(lastAudit(calls)?.get("q")).toBe("report");
    expect(lastAudit(calls)?.get("ip")).toBe("10.0.0.1");

    await userEvent.click(screen.getByRole("button", { name: "Сбросить" }));
    await waitFor(() => expect(currentUrl()).toBe("/admin?tab=audit"));
    expect(lastAudit(calls)?.toString()).toBe("page=1&size=50");
    expect(screen.getByRole("combobox", { name: "Пользователь" })).toHaveValue("");
    expect(screen.queryByRole("button", { name: "Сбросить" })).not.toBeInTheDocument();
  });

  it("says when the IP filter is not an address", async () => {
    mockBackend({
      ...asAdmin,
      "GET /api/v1/admin/audit": (url) =>
        url.searchParams.has("ip") ? Response.json({ code: "invalid_request", message: "ip" }, { status: 400 }) : auditPage(url, 1),
    });
    renderApp("/admin?tab=audit&ip=nope");
    expect(await screen.findByText("Нужен адрес или сеть, например 10.0.0.0/8")).toBeInTheDocument();
  });

  it("shows what the system runs with", async () => {
    mockBackend({
      ...asAdmin,
      "GET /api/v1/admin/system": () =>
        Response.json({
          version: "1.2.3",
          passwordLogin: true,
          providers: [{ slug: "kc", name: "Keycloak", issuer: "https://id.example.com/realms/cloud" }],
          editor: { configured: true, available: false, url: "http://collabora:9980/hosting/discovery" },
          defaultQuotaBytes: 10 * 1024 ** 3,
          auditRetentionDays: 365,
          trashRetentionDays: 0,
        }),
    });
    renderApp("/admin?tab=system");
    expect(await screen.findByText("Keycloak")).toBeInTheDocument();
    expect(screen.getByText("Не отвечает")).toBeInTheDocument();
    expect(screen.getByText("Квота новых пользователей: 10 ГБ")).toBeInTheDocument();
    expect(screen.getByText("Корзина хранится: бессрочно")).toBeInTheDocument();
    expect(screen.getByText("Журнал хранится: 365 дн.")).toBeInTheDocument();
  });

  it("fits users into one column on a phone, dialogs taking the whole screen", async () => {
    stubMedia(PHONE_MEDIA);
    mockBackend({
      ...asAdmin,
      "GET /api/v1/admin/users": () => Response.json({ items: [account("a1", "bob", { displayName: "Боб", email: "bob@example.com", isAdmin: true })], nextCursor: null }),
    });
    renderApp("/admin");
    const table = await screen.findByRole("table");
    expect(within(table).queryByRole("columnheader", { name: "Место" })).not.toBeInTheDocument();
    expect(within(table).queryByRole("columnheader", { name: "Статус" })).not.toBeInTheDocument();
    // Login, usage and badges all under the name, in its cell.
    const cell = within(table).getByText("Боб").closest("td");
    if (!cell) throw new Error("no cell");
    expect(within(cell).getByText("bob · bob@example.com")).toBeInTheDocument();
    expect(within(cell).getByText("1 ГБ из 10 ГБ")).toBeInTheDocument();
    expect(within(cell).getByText("Активен")).toBeInTheDocument();
    expect(within(cell).getByText("Администратор")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Новый пользователь" }));
    expect(await screen.findByRole("dialog", { name: "Новый пользователь" })).toHaveAttribute("data-full-screen");
  });

  it("shows the audit log as one column on a phone", async () => {
    stubMedia(PHONE_MEDIA);
    mockBackend({
      ...asAdmin,
      "GET /api/v1/admin/audit": (url) => auditPage(url, 1, [entry(1, "admin.user.update", "bob")]),
    });
    renderApp("/admin?tab=audit");
    const table = await screen.findByRole("table");
    expect(within(table).queryByRole("columnheader")).not.toBeInTheDocument();
    const cell = within(table).getByText("Пользователь изменён").closest("td");
    if (!cell) throw new Error("no cell");
    expect(cell).toHaveTextContent("Пользователь изменён · bob");
    expect(cell).toHaveTextContent("· root");
  });

  it("folds the audit filters behind a button on a phone", async () => {
    stubMedia(PHONE_MEDIA);
    mockBackend({
      ...asAdmin,
      "GET /api/v1/admin/audit": (url) => auditPage(url, 1, [entry(1, "admin.user.update", "bob")]),
    });
    renderApp("/admin?tab=audit&action=auth.&ip=10.0.0.0%2F8");
    await screen.findByRole("table");
    // Folded, it counts the filters in use.
    const toggle = screen.getByRole("button", { name: "Фильтры: 2" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("textbox", { name: "IP" })).not.toBeInTheDocument();
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(await screen.findByRole("textbox", { name: "IP" })).toHaveValue("10.0.0.0/8");
    await userEvent.click(screen.getByRole("button", { name: "Сбросить" }));
    expect(screen.getByRole("button", { name: "Фильтры" })).toBeInTheDocument();
  });
});
