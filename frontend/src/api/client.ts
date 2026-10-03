/**
 * Typed-ish fetch wrapper for the AcreSync HTTP API.
 * - Base URL comes from VITE_API_BASE (mock has no /v1, real backend has /v1).
 * - Auth: `Authorization: Bearer <token>`; mock accepts any non-empty token.
 * - Operator persona against the mock: `X-Mock-Role` header.
 * - Every POST carries an Idempotency-Key (one per user intent, reused on retry).
 * - Pollable GETs support conditional requests via ETag.
 */
export const API_BASE =
  (import.meta.env.VITE_API_BASE as string | undefined) ?? 'http://127.0.0.1:4010';

export interface ApiEnvelopeError {
  error: { code: string; message: string; requestId?: string };
}

export class ApiError extends Error {
  status: number;
  code: string;
  requestId?: string;
  replayed = false;
  constructor(status: number, code: string, message: string, requestId?: string) {
    super(message);
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

export interface ApiResult<T> {
  data: T;
  etag: string | null;
  replayed: boolean;
  status: number;
}

interface CallOptions {
  method?: 'GET' | 'POST';
  body?: unknown;
  token?: string | null;
  mockRole?: string | null;
  idempotencyKey?: string;
  etag?: string | null;
}

export function newIdempotencyKey(): string {
  return crypto.randomUUID().replace(/-/g, '');
}

export async function apiFetch<T>(path: string, opts: CallOptions = {}): Promise<ApiResult<T>> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (opts.token) headers.Authorization = `Bearer ${opts.token}`;
  if (opts.mockRole) headers['X-Mock-Role'] = opts.mockRole;
  if (opts.method === 'POST') {
    headers['Content-Type'] = 'application/json';
    headers['Idempotency-Key'] = opts.idempotencyKey ?? newIdempotencyKey();
  }
  if (opts.etag) headers['If-None-Match'] = opts.etag;

  const res = await fetch(`${API_BASE}${path}`, {
    method: opts.method ?? 'GET',
    headers,
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
  });

  if (res.status === 304) {
    return { data: undefined as T, etag: opts.etag ?? null, replayed: false, status: 304 };
  }

  const etag = res.headers.get('ETag');
  const replayed = res.headers.get('Idempotency-Replayed') === 'true';

  let payload: unknown = null;
  const text = await res.text();
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = text;
    }
  }

  if (!res.ok) {
    const env = payload as ApiEnvelopeError | null;
    const code = (env && env.error && env.error.code) || `http_${res.status}`;
    const message = (env && env.error && env.error.message) || `Request failed (${res.status})`;
    const requestId = env && env.error ? env.error.requestId : undefined;
    throw new ApiError(res.status, code, message, requestId);
  }

  return { data: payload as T, etag, replayed, status: res.status };
}

/** Shape of a paginated list in this API. */
export interface Page<T> {
  items?: T[];
  data?: T[];
  nextCursor?: string | null;
}

/** Extract array from either {items} or {data} or a bare array. */
export function pageItems<T>(page: Page<T> | T[] | null | undefined): T[] {
  if (!page) return [];
  if (Array.isArray(page)) return page;
  return page.items ?? page.data ?? [];
}
