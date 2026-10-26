import { describe, it, expect, afterEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { vi } from "vitest";
import { PHONE_MEDIA } from "./media";
import { currentUrl, mockBackend, renderApp, stubMedia, unauthorized } from "./testing";

afterEach(() => {
  vi.unstubAllGlobals();
});

const alice = { id: "u1", username: "alice", displayName: "Алиса", isAdmin: false };
const providers = () => Response.json({ password: true, providers: [{ slug: "kc", name: "Keycloak" }] });
const emptyRoot = {
  "GET /api/v1/nodes/root": () => Response.json({ id: "r", name: "", kind: "dir", size: 0, etag: "e", mtime: "2026-10-24T10:00:00Z", permissions: ["read", "write", "reshare"], path: [{ id: "r", name: "" }] }),
  "GET /api/v1/nodes/root/children": () => Response.json({ items: [], nextCursor: null }),
};

describe("App", () => {
  it("sends a signed-out visitor to the login page and back after signing in", async () => {
    let signedIn = false;
    const calls = mockBackend({
      "GET /api/v1/auth/me": () => (signedIn ? Response.json(alice) : unauthorized()),
      "GET /api/v1/auth/providers": providers,
      "POST /api/v1/auth/login": () => {
        signedIn = true;
        return Response.json(alice);
      },
      ...emptyRoot,
    });
    renderApp("/files?sort=size");
    expect(await screen.findByText("Вход в Облако")).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "Войти через Keycloak" })).toHaveAttribute(
      "href",
      "/api/v1/auth/oidc/kc/start?next=%2Ffiles%3Fsort%3Dsize",
    );

    await userEvent.type(screen.getByLabelText(/Логин/), "alice");
    await userEvent.type(screen.getByLabelText(/Пароль/), "secret");
    await userEvent.click(screen.getByRole("button", { name: "Войти" }));
    expect(await screen.findByText("Здесь появятся ваши файлы")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST")?.body).toEqual({ username: "alice", password: "secret" });
    expect(screen.getByText("Алиса")).toBeInTheDocument();
  });

  it("shows why a sign-in failed", async () => {
    mockBackend({
      "GET /api/v1/auth/me": unauthorized,
      "GET /api/v1/auth/providers": providers,
      "POST /api/v1/auth/login": () => Response.json({ code: "invalid_credentials", message: "nope" }, { status: 401 }),
    });
    renderApp("/login");
    await userEvent.type(await screen.findByLabelText(/Логин/), "alice");
    await userEvent.type(screen.getByLabelText(/Пароль/), "wrong");
    await userEvent.click(screen.getByRole("button", { name: "Войти" }));
    expect(await screen.findByText("Неверное имя пользователя или пароль.")).toBeInTheDocument();
  });

  it("shows a provider flow's error from ?error=", async () => {
    mockBackend({ "GET /api/v1/auth/me": unauthorized, "GET /api/v1/auth/providers": providers });
    renderApp("/login?error=signup_disabled");
    expect(await screen.findByText(/нет доступа к облаку/)).toBeInTheDocument();
  });

  it("signs out", async () => {
    let signedIn = true;
    mockBackend({
      "GET /api/v1/auth/me": () => (signedIn ? Response.json(alice) : unauthorized()),
      "GET /api/v1/auth/providers": providers,
      "POST /api/v1/auth/logout": () => {
        signedIn = false;
        return new Response(null, { status: 204 });
      },
      ...emptyRoot,
    });
    renderApp("/files");
    await userEvent.click(await screen.findByRole("button", { name: "Меню пользователя" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Выйти" }));
    expect(await screen.findByText("Вход в Облако")).toBeInTheDocument();
    expect(screen.queryByText("Алиса")).not.toBeInTheDocument();
  });

  it("renders NotFound for an unknown route", async () => {
    mockBackend({ "GET /api/v1/auth/me": unauthorized });
    renderApp("/no/such/page");
    expect(screen.getByText("Страница не найдена")).toBeInTheDocument();
    expect(screen.getByText(/\/no\/such\/page/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "На главную" })).toHaveAttribute("href", "/");
  });

  it("shows the backend as available when /readyz succeeds", async () => {
    mockBackend({ "GET /api/v1/auth/me": unauthorized });
    renderApp("/no/such/page");
    expect(await screen.findByText("Сервер доступен")).toBeInTheDocument();
  });

  it("shows the backend as unavailable when /readyz reports not ready", async () => {
    mockBackend({
      "GET /api/v1/auth/me": unauthorized,
      "GET /readyz": () => Response.json({ code: "database_unavailable", message: "db not reachable" }, { status: 503 }),
    });
    renderApp("/no/such/page");
    expect(await screen.findByText("Сервер недоступен")).toBeInTheDocument();
  });

  it("shows the backend as unavailable when the network is down", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("Failed to fetch");
      }),
    );
    renderApp("/no/such/page");
    expect(await screen.findByText("Сервер недоступен")).toBeInTheDocument();
  });
});

describe("App on a phone", () => {
  const empty = { "GET /api/v1/trash": () => Response.json({ items: [], nextCursor: null }) };

  it("puts the sections behind a burger, and closes the drawer on navigation", async () => {
    stubMedia(PHONE_MEDIA);
    mockBackend({ "GET /api/v1/auth/me": () => Response.json(alice), ...emptyRoot, ...empty });
    renderApp("/files");
    await userEvent.click(await screen.findByRole("button", { name: "Меню" }));
    const drawer = await screen.findByRole("dialog");
    const nav = within(drawer).getByRole("navigation", { name: "Разделы" });
    expect(within(nav).getAllByRole("link").map((l) => l.textContent)).toEqual(["Файлы", "Доступно мне", "Корзина", "Настройки"]);
    expect(within(nav).getByRole("link", { name: "Файлы" })).toHaveAttribute("aria-current", "page");
    expect(within(drawer).getByText("Алиса")).toBeInTheDocument();
    expect(within(drawer).getByRole("button", { name: "Выйти" })).toBeInTheDocument();
    expect(await within(drawer).findByText("Сервер доступен")).toBeInTheDocument();
    // The desktop's row of sections and user menu aren't there.
    expect(screen.queryByRole("button", { name: "Меню пользователя" })).not.toBeInTheDocument();

    await userEvent.click(within(nav).getByRole("link", { name: "Корзина" }));
    expect(currentUrl()).toBe("/trash");
    await waitFor(() => expect(screen.queryByRole("navigation", { name: "Разделы" })).not.toBeInTheDocument());
  });

  it("lists Админка for an admin", async () => {
    stubMedia(PHONE_MEDIA);
    mockBackend({ "GET /api/v1/auth/me": () => Response.json({ ...alice, isAdmin: true }), ...emptyRoot });
    renderApp("/files");
    await userEvent.click(await screen.findByRole("button", { name: "Меню" }));
    const nav = within(await screen.findByRole("dialog")).getByRole("navigation", { name: "Разделы" });
    expect(within(nav).getByRole("link", { name: "Админка" })).toHaveAttribute("href", "/admin");
  });
});
