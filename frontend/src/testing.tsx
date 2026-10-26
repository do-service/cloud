import { MantineProvider } from "@mantine/core";
import { Notifications } from "@mantine/notifications";
import { QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import { vi } from "vitest";
import App from "./App";
import { t } from "./i18n";
import { createQueryClient } from "./queryClient";
import { theme } from "./theme";

type Handler = (url: URL, init: RequestInit | undefined) => Response | Promise<Response>;

export type Call = { method: string; url: URL; body: unknown };

// A fake backend behind fetch: routes are "METHOD /path" (no query) →
// handler. /readyz answers OK unless overridden; anything else unrouted is
// the backend's 404, so a test sees a request it didn't expect fail loudly.
export function mockBackend(routes: Record<string, Handler>): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://localhost");
      const method = init?.method ?? "GET";
      calls.push({ method, url, body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined });
      const handler = routes[`${method} ${url.pathname}`];
      if (handler) return handler(url, init);
      if (url.pathname === "/readyz") return new Response("ok");
      return Response.json({ code: "not_found", message: `no route ${method} ${url.pathname}` }, { status: 404 });
    }),
  );
  return calls;
}

export const unauthorized = () => Response.json({ code: "unauthorized", message: "sign in required" }, { status: 401 });

// Makes matchMedia match exactly the given queries (jsdom's matches none),
// e.g. PHONE_MEDIA for a phone; undone by vi.unstubAllGlobals.
export function stubMedia(...matching: string[]) {
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: matching.includes(query),
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  }));
}

let location = "";

function LocationProbe() {
  const l = useLocation();
  location = l.pathname + l.search;
  return null;
}

// The app's current URL (path and query), for tests of what it keeps there.
export const currentUrl = () => location;

export function renderApp(path: string) {
  return render(
    <QueryClientProvider client={createQueryClient(false)}>
      <MantineProvider theme={theme}>
        <Notifications />
        <MemoryRouter initialEntries={[path]}>
          <App />
          <LocationProbe />
        </MemoryRouter>
      </MantineProvider>
    </QueryClientProvider>,
  );
}

// Opens the toolbar's menu of the selection's actions, unless it is open.
export async function openSelectionActions() {
  const button = screen.getByRole("button", { name: t("files.actions") });
  if (button.getAttribute("aria-expanded") === "true") return;
  await userEvent.click(button);
}

// Picks one of the selection's actions from the toolbar's menu.
export async function selectionAction(name: string) {
  await openSelectionActions();
  // The menu opens with a transition: slower than findBy's second when the
  // whole suite runs at once.
  await userEvent.click(await screen.findByRole("menuitem", { name }, { timeout: 5000 }));
}
