import { useEffect, useRef, useState } from 'react';
import { apiFetch, type ApiError } from './client';

interface PollState<T> {
  data: T | null;
  loading: boolean;
  error: ApiError | null;
  etag: string | null;
  refresh: () => void;
}

/**
 * Poll a GET endpoint every `intervalMs` while mounted.
 * Uses conditional requests so an unchanged poll is nearly free (304).
 * Pass `intervalMs: 0` to fetch once.
 */
export function usePoll<T>(path: string | null, opts?: { intervalMs?: number; token?: string | null; mockRole?: string | null }): PollState<T> {
  const { intervalMs = 0, token = null, mockRole = null } = opts ?? {};
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState<boolean>(!!path);
  const [error, setError] = useState<ApiError | null>(null);
  const etagRef = useRef<string | null>(null);
  const [etag, setEtag] = useState<string | null>(null);
  const [tick, setTick] = useState(0);

  useEffect(() => {
    if (!path) {
      setLoading(false);
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const res = await apiFetch<T>(path, { token, mockRole, etag: etagRef.current });
        if (cancelled) return;
        if (res.status !== 304) {
          setData(res.data);
          etagRef.current = res.etag;
          setEtag(res.etag);
        }
        setError(null);
      } catch (e) {
        if (!cancelled) setError(e as ApiError);
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, tick, token, mockRole]);

  useEffect(() => {
    if (!path || !intervalMs) return;
    const id = setInterval(() => setTick((t) => t + 1), intervalMs);
    return () => clearInterval(id);
  }, [path, intervalMs]);

  return { data, loading, error, etag, refresh: () => setTick((t) => t + 1) };
}
