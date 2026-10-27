// Thin fetch wrapper for the admin API. Every request is same-origin
// (served from the same binary under /admin/), so cookies go along for free
// with credentials: "same-origin"; mutating requests also carry the CSRF
// header the server requires (see internal/adminapi/middleware.go).

const CSRF_HEADER = "X-CI-Cache-Admin"

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const isMutating = method !== "GET" && method !== "HEAD"
  const headers: Record<string, string> = {}
  if (body !== undefined) headers["Content-Type"] = "application/json"
  if (isMutating) headers[CSRF_HEADER] = "1"

  const res = await fetch(`/admin/api${path}`, {
    method,
    headers,
    credentials: "same-origin",
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })

  if (!res.ok) {
    let message = res.statusText
    try {
      const data = await res.json()
      if (data?.error) message = data.error
    } catch {
      // response had no JSON body; fall back to statusText
    }
    throw new ApiError(res.status, message)
  }

  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

export const api = {
  get: <T>(path: string) => request<T>("GET", path),
  post: <T>(path: string, body?: unknown) => request<T>("POST", path, body ?? {}),
  put: <T>(path: string, body?: unknown) => request<T>("PUT", path, body ?? {}),
  del: <T>(path: string) => request<T>("DELETE", path),
}

export interface User {
  id: number
  email: string
  createdAt: string
}

export interface Token {
  id: number
  name: string
  scope: "read" | "write"
  createdAt: string
  lastUsedAt?: string
  revokedAt?: string
}

export interface CacheEntry {
  // Storage key, e.g. "nx/<hash>"; identifies the entry for deletes.
  key: string
  protocol: string
  hash: string
  // Stored before keys were namespaced; still served to Nx clients.
  legacy?: boolean
  size: number
  modTime: string
  readCount: number
  lastReadAt?: string
}

export interface ListCacheResponse {
  entries: CacheEntry[] | null
  nextCursor?: string
}

export type StorageBackendType = "local" | "s3" | "gcs"

export interface Settings {
  storageBackend: StorageBackendType
  localDir: string
  s3Bucket: string
  s3Region: string
  s3Prefix: string
  s3Endpoint: string
  s3UsePathStyle: boolean
  s3AccessKeyIdSet: boolean
  s3SecretAccessKeySet: boolean
  gcsBucket: string
  gcsPrefix: string
  gcsCredentialsSet: boolean
  sessionTtlSeconds: number
  maxCacheEntryBytes: number
  updatedAt: string
}

// Secret fields are omitted entirely to mean "leave unchanged"; set to ""
// to explicitly clear; set to a non-empty value to update. Never send the
// existing secret back — the server never returns it, so the UI can't.
export interface UpdateSettingsRequest {
  storageBackend: StorageBackendType
  localDir: string
  s3Bucket: string
  s3Region: string
  s3Prefix: string
  s3Endpoint: string
  s3UsePathStyle: boolean
  s3AccessKeyId?: string
  s3SecretAccessKey?: string
  gcsBucket: string
  gcsPrefix: string
  gcsCredentialsJson?: string
  sessionTtlSeconds: number
  maxCacheEntryBytes: number
}
