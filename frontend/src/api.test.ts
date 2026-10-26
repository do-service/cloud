import { describe, it, expect, vi, afterEach } from "vitest";
import { ApiError, apiRequest, checkReady, errorText } from "./api";

function mockFetch(impl: () => Promise<Response>) {
  const fn = vi.fn(impl);
  vi.stubGlobal("fetch", fn);
  return fn;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

async function caught(promise: Promise<unknown>): Promise<ApiError> {
  try {
    await promise;
  } catch (err) {
    if (err instanceof ApiError) return err;
    throw err;
  }
  throw new Error("expected the request to fail");
}

describe("apiRequest", () => {
  it("prefixes /api/v1, sends JSON and parses a JSON reply", async () => {
    const fetchMock = mockFetch(async () => Response.json({ ok: true }));
    await expect(apiRequest("/things", { method: "POST", body: { a: 1 } })).resolves.toEqual({ ok: true });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/things",
      expect.objectContaining({ method: "POST", credentials: "same-origin", body: '{"a":1}' }),
    );
  });

  it("throws ApiError with the backend's code and message", async () => {
    mockFetch(async () => Response.json({ code: "not_found", message: "file not found" }, { status: 404 }));
    const err = await caught(apiRequest("/files/x"));
    expect(err.status).toBe(404);
    expect(err.code).toBe("not_found");
    expect(err.message).toBe("file not found");
    expect(errorText(err)).toBe("Запрошенный ресурс не найден.");
  });

  it("maps a non-JSON error body to code unknown", async () => {
    mockFetch(async () => new Response("<html>Bad Gateway</html>", { status: 502 }));
    const err = await caught(apiRequest("/files"));
    expect(err.status).toBe(502);
    expect(err.code).toBe("unknown");
  });

  it("maps a network failure to status 0, code network", async () => {
    mockFetch(async () => {
      throw new TypeError("Failed to fetch");
    });
    const err = await caught(apiRequest("/files"));
    expect(err.status).toBe(0);
    expect(err.code).toBe("network");
    expect(errorText(err)).toBe("Нет связи с сервером. Проверьте подключение к сети.");
  });
});

describe("checkReady", () => {
  it("calls /readyz outside the versioned API and accepts a plain-text reply", async () => {
    const fetchMock = mockFetch(async () => new Response("ok"));
    await expect(checkReady()).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledWith("/readyz", expect.anything());
  });
});

describe("errorText", () => {
  it("falls back to the generic text for unknown codes and foreign errors", () => {
    const generic = "Что-то пошло не так. Попробуйте ещё раз.";
    expect(errorText(new ApiError(418, "teapot", "x"))).toBe(generic);
    expect(errorText(new Error("boom"))).toBe(generic);
    expect(errorText("boom")).toBe(generic);
  });
});
