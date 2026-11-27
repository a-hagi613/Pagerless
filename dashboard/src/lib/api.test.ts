import { afterEach, describe, expect, it, vi } from "vitest";

import {
  ApiError,
  createPage,
  exchangeSession,
  getDiff,
  getEvents,
  getEvidence,
  getPage,
  listPages,
  replyToPage,
  setUnauthorizedHandler,
  stopTurn,
  UnauthenticatedError,
} from "./api";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  setUnauthorizedHandler(null);
});

describe("session exchange", () => {
  it("posts the operator token once and never stores it", async () => {
    const storage = { setItem: vi.fn(), getItem: vi.fn() };
    vi.stubGlobal("localStorage", storage);
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    await exchangeSession("operator-token-value");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/v1/session");
    expect(init.method).toBe("POST");
    expect(init.body).toBe(JSON.stringify({ token: "operator-token-value" }));
    expect(storage.setItem).not.toHaveBeenCalled();
  });

  it("turns a refused session into UnauthenticatedError and notifies the provider", async () => {
    const handler = vi.fn();
    setUnauthorizedHandler(handler);
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse({ error: { code: "unauthenticated", message: "no" } }, 401)));

    await expect(exchangeSession("bad")).rejects.toBeInstanceOf(UnauthenticatedError);
    expect(handler).toHaveBeenCalledOnce();
  });
});

describe("investigation writes", () => {
  it("sends the documented hint object and an Idempotency-Key", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ pageId: "p1", turnId: "t1" }, 201));
    vi.stubGlobal("fetch", fetchMock);

    const answer = await createPage(
      { message: "Why is checkout slow?", namespace: "shop", target: { kind: "Deployment", name: "checkout" } },
      "idem-1",
    );

    expect(answer).toEqual({ pageId: "p1", turnId: "t1" });
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/v1/pages");
    const headers = new Headers(init.headers);
    expect(headers.get("Idempotency-Key")).toBe("idem-1");
    expect(JSON.parse(String(init.body))).toEqual({
      message: "Why is checkout slow?",
      namespace: "shop",
      target: { kind: "Deployment", name: "checkout" },
    });
  });

  it("uses the reply route and forwards a conflict message", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        jsonResponse({ error: { code: "page_busy", message: "a turn is active" } }, 409),
      ),
    );

    await expect(replyToPage("p1", { message: "again" }, "idem-2")).rejects.toMatchObject({
      status: 409,
      code: "page_busy",
      message: "a turn is active",
    });
  });

  it("stops through the documented route", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: "stopped" }));
    vi.stubGlobal("fetch", fetchMock);

    expect(await stopTurn("p/1")).toEqual({ status: "stopped" });
    expect((fetchMock.mock.calls[0] as [string])[0]).toBe("/api/v1/pages/p%2F1/stop");
  });
});

// User-visible capabilities map one-to-one onto the shared contract rather than a second command table.
// The browser and CLI therefore see the same pages, turns, replay cursor, evidence, diff and stop outcome.
describe("client parity with the shared contract", () => {
  it("lists, shows, follows and replays through the documented paths", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ pages: [] }))
      .mockResolvedValueOnce(jsonResponse({ id: "p1", messages: [], lastEventId: 7 }))
      .mockResolvedValueOnce(jsonResponse({ events: [{ seq: 8 }] }))
      .mockResolvedValueOnce(jsonResponse({ files: [], prUrl: "https://example.test/pr/1" }));
    vi.stubGlobal("fetch", fetchMock);

    expect(await listPages()).toEqual({ pages: [] });
    expect((await getPage("p1")).lastEventId).toBe(7);
    expect(await getEvents("p1", 7)).toEqual([{ seq: 8 }]);
    expect(await getDiff("p1")).toEqual({ files: [], prUrl: "https://example.test/pr/1" });

    const paths = fetchMock.mock.calls.map((call) => (call as [string])[0]);
    expect(paths).toEqual([
      "/api/v1/pages",
      "/api/v1/pages/p1",
      "/api/v1/pages/p1/events?after=7",
      "/api/v1/pages/p1/diff",
    ]);
  });
});

describe("read failures", () => {  it("encodes the call id in the evidence path", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ callId: "c 1" }));
    vi.stubGlobal("fetch", fetchMock);
    await getEvidence("p1", "c 1");
    expect((fetchMock.mock.calls[0] as [string])[0]).toBe("/api/v1/pages/p1/evidence/c%201");
  });

  it("surfaces a non-JSON refusal with its status line", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation(
        () => Promise.resolve(new Response("nope", { status: 503, statusText: "Service Unavailable" })),
      ),
    );
    await expect(stopTurn("p1")).rejects.toBeInstanceOf(ApiError);
    await expect(stopTurn("p1")).rejects.toThrow("503 Service Unavailable");
  });
});
