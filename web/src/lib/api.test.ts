import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { api, ApiError } from "@/lib/api"

function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  })
}

describe("api", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn())
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it("GET hits the admin API path without a CSRF header or body", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(200, { id: 1 }))

    await api.get("/auth/me")

    expect(fetch).toHaveBeenCalledWith(
      "/admin/api/auth/me",
      expect.objectContaining({
        method: "GET",
        credentials: "same-origin",
        body: undefined,
      })
    )
    const [, init] = vi.mocked(fetch).mock.calls[0]
    const headers = init?.headers as Record<string, string> | undefined
    expect(headers?.["X-CI-Cache-Admin"]).toBeUndefined()
  })

  it("POST sends the CSRF header and a JSON body", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(200, { ok: true }))

    await api.post("/tokens", { name: "ci" })

    const [url, init] = vi.mocked(fetch).mock.calls[0]
    expect(url).toBe("/admin/api/tokens")
    expect(init?.method).toBe("POST")
    const headers = init?.headers as Record<string, string> | undefined
    expect(headers?.["X-CI-Cache-Admin"]).toBe("1")
    expect(init?.body).toBe(JSON.stringify({ name: "ci" }))
  })

  it("PUT sends the CSRF header and a JSON body", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(200, { ok: true }))

    await api.put("/settings", { storageBackend: "local" })

    const [url, init] = vi.mocked(fetch).mock.calls[0]
    expect(url).toBe("/admin/api/settings")
    expect(init?.method).toBe("PUT")
    const headers = init?.headers as Record<string, string> | undefined
    expect(headers?.["X-CI-Cache-Admin"]).toBe("1")
    expect(init?.body).toBe(JSON.stringify({ storageBackend: "local" }))
  })

  it("defaults a missing POST body to {} so a CSRF-only call still sends JSON", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(200, {}))

    await api.post("/auth/logout")

    const [, init] = vi.mocked(fetch).mock.calls[0]
    expect(init?.body).toBe("{}")
  })

  it("defaults a missing PUT body to {} as well", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(200, {}))

    await api.put("/settings")

    const [, init] = vi.mocked(fetch).mock.calls[0]
    expect(init?.body).toBe("{}")
  })

  it("returns undefined for a 204 response instead of parsing a body", async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(null, { status: 204 }))

    await expect(api.del("/tokens/1")).resolves.toBeUndefined()
  })

  it("throws ApiError with the server's error message on failure", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(401, { error: "not authenticated" }))

    await expect(api.get("/auth/me")).rejects.toMatchObject(new ApiError(401, "not authenticated"))
  })

  it("falls back to statusText when the error response's JSON body has no error field", async () => {
    vi.mocked(fetch).mockResolvedValue(
      new Response(JSON.stringify({ detail: "unrelated field" }), {
        status: 500,
        statusText: "Internal Server Error",
        headers: { "Content-Type": "application/json" },
      })
    )

    await expect(api.get("/auth/me")).rejects.toMatchObject(
      new ApiError(500, "Internal Server Error")
    )
  })

  it("falls back to statusText when the error response has no JSON body", async () => {
    vi.mocked(fetch).mockResolvedValue(
      new Response("not json", { status: 500, statusText: "Internal Server Error" })
    )

    await expect(api.get("/auth/me")).rejects.toMatchObject(
      new ApiError(500, "Internal Server Error")
    )
  })
})
