import { afterEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { isLoopbackRedirect } from "../devices";
import { currentUrl, mockBackend, renderApp, unauthorized } from "../testing";

const leaveTo = vi.hoisted(() => vi.fn());
vi.mock("../devices", async (importOriginal) => ({ ...(await importOriginal<typeof import("../devices")>()), leaveTo }));

afterEach(() => {
  vi.unstubAllGlobals();
  leaveTo.mockReset();
});

const me = () => Response.json({ id: "u1", username: "vasily", displayName: "Василий", isAdmin: false });
const challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM";

function authorizeUrl(override: Record<string, string> = {}): string {
  const q = new URLSearchParams({
    redirect_uri: "http://127.0.0.1:53682/cb",
    code_challenge: challenge,
    code_challenge_method: "S256",
    state: "xyz&1",
    name: "Ноутбук Василия",
    platform: "macos",
    client_version: "0.1.0",
    ...override,
  });
  return `/devices/authorize?${q.toString()}`;
}

const sentRequest = {
  redirectUri: "http://127.0.0.1:53682/cb",
  codeChallenge: challenge,
  codeChallengeMethod: "S256",
  state: "xyz&1",
  name: "Ноутбук Василия",
  platform: "macos",
  clientVersion: "0.1.0",
};

describe("DeviceAuthorizePage", () => {
  it("shows the device, account and server, and allowing goes to the app", async () => {
    const redirect = "http://127.0.0.1:53682/cb?code=abc&state=xyz%261";
    const calls = mockBackend({
      "GET /api/v1/auth/me": me,
      "POST /api/v1/devices/authorize": () => Response.json({ redirect }),
    });
    renderApp(authorizeUrl());
    expect(await screen.findByRole("heading", { name: "Подключить приложение?" })).toBeInTheDocument();
    const table = within(screen.getByRole("table"));
    expect(table.getByText("Ноутбук Василия")).toBeInTheDocument();
    expect(table.getByText("macOS · версия 0.1.0")).toBeInTheDocument();
    expect(table.getByText("Василий")).toBeInTheDocument();
    expect(table.getByText("vasily")).toBeInTheDocument();
    expect(table.getByText(window.location.origin)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Разрешить" }));
    await waitFor(() => expect(leaveTo).toHaveBeenCalledWith(redirect));
    expect(calls.find((c) => c.method === "POST")?.body).toEqual(sentRequest);
    expect(await screen.findByText("Готово")).toBeInTheDocument();
  });

  it("refusing sends the app access_denied", async () => {
    const redirect = "http://127.0.0.1:53682/cb?error=access_denied&state=xyz%261";
    const calls = mockBackend({
      "GET /api/v1/auth/me": me,
      "POST /api/v1/devices/authorize": () => Response.json({ redirect }),
    });
    renderApp(authorizeUrl());
    await userEvent.click(await screen.findByRole("button", { name: "Отказать" }));
    await waitFor(() => expect(leaveTo).toHaveBeenCalledWith(redirect));
    expect(calls.find((c) => c.method === "POST")?.body).toEqual({ ...sentRequest, deny: true });
    expect(await screen.findByText("Вход отменён")).toBeInTheDocument();
  });

  it("shows the server's refusal and stays", async () => {
    mockBackend({
      "GET /api/v1/auth/me": me,
      "POST /api/v1/devices/authorize": () => Response.json({ code: "device_limit", message: "limit" }, { status: 409 }),
    });
    renderApp(authorizeUrl());
    await userEvent.click(await screen.findByRole("button", { name: "Разрешить" }));
    expect(await screen.findByText(/Подключено слишком много устройств/)).toBeInTheDocument();
    expect(leaveTo).not.toHaveBeenCalled();
  });

  it("doesn't follow a redirect off the app's address", async () => {
    mockBackend({
      "GET /api/v1/auth/me": me,
      "POST /api/v1/devices/authorize": () => Response.json({ redirect: "https://evil.example/?code=abc" }),
    });
    renderApp(authorizeUrl());
    await userEvent.click(await screen.findByRole("button", { name: "Разрешить" }));
    expect(await screen.findByText("Что-то пошло не так. Попробуйте ещё раз.")).toBeInTheDocument();
    expect(leaveTo).not.toHaveBeenCalled();
  });

  it.each([
    ["a localhost redirect", { redirect_uri: "http://localhost:53682/cb" }],
    ["a remote redirect", { redirect_uri: "https://evil.example/cb" }],
    ["a redirect with a query", { redirect_uri: "http://127.0.0.1:53682/cb?x=1" }],
    ["a low port", { redirect_uri: "http://127.0.0.1:80/cb" }],
    ["the plain method", { code_challenge_method: "plain" }],
    ["a short challenge", { code_challenge: challenge.slice(1) }],
    ["no state", { state: "" }],
    ["a long state", { state: "s".repeat(201) }],
    ["no name", { name: "  " }],
    ["another platform", { platform: "android" }],
  ])("shows an error for %s and never asks", async (_, override) => {
    const calls = mockBackend({ "GET /api/v1/auth/me": me });
    renderApp(authorizeUrl(override));
    expect(await screen.findByText("Неверная ссылка для входа")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Разрешить" })).not.toBeInTheDocument();
    expect(calls.some((c) => c.method === "POST")).toBe(false);
  });

  it("sends a signed-out user to sign in and back, query and all", async () => {
    let signedIn = false;
    mockBackend({
      "GET /api/v1/auth/me": () => (signedIn ? me() : unauthorized()),
      "GET /api/v1/auth/providers": () => Response.json({ password: true, providers: [{ slug: "kc", name: "Keycloak" }] }),
      "POST /api/v1/auth/login": () => {
        signedIn = true;
        return me();
      },
    });
    const target = authorizeUrl();
    renderApp(target);
    expect(await screen.findByRole("heading", { name: "Вход в Облако" })).toBeInTheDocument();
    expect(currentUrl()).toBe(`/login?next=${encodeURIComponent(target)}`);
    // The provider's flow carries the same return address.
    expect(await screen.findByRole("link", { name: "Войти через Keycloak" })).toHaveAttribute(
      "href",
      `/api/v1/auth/oidc/kc/start?next=${encodeURIComponent(target)}`,
    );

    await userEvent.type(screen.getByLabelText(/Логин/), "vasily");
    await userEvent.type(screen.getByLabelText(/Пароль/), "secret");
    await userEvent.click(screen.getByRole("button", { name: "Войти" }));
    expect(await screen.findByRole("heading", { name: "Подключить приложение?" })).toBeInTheDocument();
    expect(currentUrl()).toBe(target);
  });
});

describe("isLoopbackRedirect", () => {
  it.each([
    ["http://127.0.0.1:53682/cb", true],
    ["http://[::1]:53682/cb", true],
    ["http://127.0.0.1:1024/", true],
    ["http://127.0.0.1:53682", false],
    ["http://127.0.0.1:65536/cb", false],
    ["http://127.0.0.1:053682/cb", false],
    ["http://127.0.0.1:53682/cb#f", false],
    ["http://127.0.0.1:53682/a b", false],
    ["http://127.0.0.1:53682@evil/cb", false],
  ])("%s → %s", (raw, ok) => {
    expect(isLoopbackRedirect(raw)).toBe(ok);
  });
});
