// HTTP access to a Lux server. Runs only in the main process: the token
// never reaches the renderer.
import type { LuxError } from "../shared/ipc";

export class ApiError extends Error implements LuxError {
  constructor(public status: number, public code: string, message: string, public details?: unknown) {
    super(message);
  }
}

export interface Connection {
  url: string;
  token: string | null;
  workspace: string;
}

const REQUEST_TIMEOUT_MS = 6 * 60_000; // importing an agent clones a repository

export async function request<T>(conn: Connection, method: string, path: string, body?: unknown, fetchImpl: typeof fetch = fetch, asText = false): Promise<T> {
  const headers: Record<string, string> = { accept: asText ? "text/plain" : "application/json" };
  if (body !== undefined) headers["content-type"] = "application/json";
  if (conn.token) headers.authorization = `Bearer ${conn.token}`;
  let res: Response;
  try {
    res = await fetchImpl(conn.url + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      redirect: "error", // never follow a redirect with the token attached
    });
  } catch (err: any) {
    throw new ApiError(0, "unreachable", `Cannot reach ${conn.url}${err?.cause?.code ? ` (${err.cause.code})` : ""}`);
  }
  const text = await res.text();
  if (asText && res.ok) return text as T; // patches; errors are still JSON
  let json: any = undefined;
  if (text) {
    try {
      json = JSON.parse(text);
    } catch {
      throw new ApiError(res.status, "bad_response", `Unexpected response from ${path}`);
    }
  }
  if (!res.ok) {
    const e = json?.error ?? {};
    throw new ApiError(res.status, e.code ?? "http_" + res.status, e.message ?? `HTTP ${res.status}`, e.details);
  }
  return json as T;
}

/** Fetch raw bytes (workspace files). Errors are the server's JSON errors. */
export async function requestBytes(conn: Connection, path: string, fetchImpl: typeof fetch = fetch): Promise<{ bytes: Uint8Array; contentType: string }> {
  const headers: Record<string, string> = {};
  if (conn.token) headers.authorization = `Bearer ${conn.token}`;
  let res: Response;
  try {
    res = await fetchImpl(conn.url + path, { headers, signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS), redirect: "error" });
  } catch (err: any) {
    throw new ApiError(0, "unreachable", `Cannot reach ${conn.url}${err?.cause?.code ? ` (${err.cause.code})` : ""}`);
  }
  if (!res.ok) {
    let e: any = {};
    try {
      e = (await res.json())?.error ?? {};
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, e.code ?? "http_" + res.status, e.message ?? `HTTP ${res.status}`);
  }
  return { bytes: new Uint8Array(await res.arrayBuffer()), contentType: res.headers.get("content-type") ?? "" };
}

/** Send a file's bytes; the response is JSON. */
export async function uploadBytes<T>(conn: Connection, path: string, bytes: Uint8Array, fetchImpl: typeof fetch = fetch): Promise<T> {
  const headers: Record<string, string> = { "content-type": "application/octet-stream", accept: "application/json" };
  if (conn.token) headers.authorization = `Bearer ${conn.token}`;
  let res: Response;
  try {
    res = await fetchImpl(conn.url + path, { method: "POST", headers, body: bytes as BodyInit, signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS), redirect: "error" });
  } catch (err: any) {
    throw new ApiError(0, "unreachable", `Cannot reach ${conn.url}${err?.cause?.code ? ` (${err.cause.code})` : ""}`);
  }
  const json: any = await res.json().catch(() => undefined);
  if (!res.ok) {
    const e = json?.error ?? {};
    throw new ApiError(res.status, e.code ?? "http_" + res.status, e.message ?? `HTTP ${res.status}`);
  }
  return json as T;
}

export const ws = (conn: Connection, path: string) => `/v1/w/${encodeURIComponent(conn.workspace || "default")}${path}`;

// ---- Server-Sent Events ----

export interface SSEMessage { event: string; id?: string; data: string }

/** Incremental parser for a text/event-stream body. */
export class SSEParser {
  private buf = "";
  private event = "";
  private id: string | undefined;
  private data: string[] = [];

  push(chunk: string): SSEMessage[] {
    this.buf += chunk;
    const out: SSEMessage[] = [];
    let nl: number;
    while ((nl = this.buf.indexOf("\n")) >= 0) {
      let line = this.buf.slice(0, nl);
      this.buf = this.buf.slice(nl + 1);
      if (line.endsWith("\r")) line = line.slice(0, -1);
      if (line === "") {
        if (this.data.length > 0 || this.event) {
          out.push({ event: this.event || "message", id: this.id, data: this.data.join("\n") });
        }
        this.event = "";
        this.data = [];
        continue;
      }
      if (line.startsWith(":")) continue;
      const colon = line.indexOf(":");
      const field = colon < 0 ? line : line.slice(0, colon);
      let value = colon < 0 ? "" : line.slice(colon + 1);
      if (value.startsWith(" ")) value = value.slice(1);
      if (field === "event") this.event = value;
      else if (field === "data") this.data.push(value);
      else if (field === "id") this.id = value;
    }
    if (this.buf.length > 8 * 1024 * 1024) throw new Error("event stream line too long");
    return out;
  }
}

export interface StreamHandlers {
  onMessage: (m: SSEMessage) => void;
  /** Called when a stream (re)connects or drops; `connected=false` means retrying. */
  onStatus?: (connected: boolean, error?: string) => void;
}

/**
 * Follow an SSE endpoint until `stop()` is called or the server ends it
 * (`end` event). Reconnects with backoff; `pathFor(lastId)` builds the URL
 * so a task stream resumes after the last event it delivered.
 */
export function follow(conn: Connection, pathFor: (lastId?: string) => string, h: StreamHandlers, fetchImpl: typeof fetch = fetch): { stop: () => void; done: Promise<void> } {
  const ac = new AbortController();
  let lastId: string | undefined;
  const done = (async () => {
    let attempt = 0;
    while (!ac.signal.aborted) {
      try {
        const res = await fetchImpl(conn.url + pathFor(lastId), {
          headers: { accept: "text/event-stream", ...(conn.token ? { authorization: `Bearer ${conn.token}` } : {}) },
          signal: ac.signal,
          redirect: "error",
        });
        if (!res.ok || !res.body) {
          if (res.status >= 400 && res.status < 500) {
            h.onStatus?.(false, `HTTP ${res.status}`);
            return; // not retryable (auth, not found)
          }
          throw new Error(`HTTP ${res.status}`);
        }
        attempt = 0;
        h.onStatus?.(true);
        const parser = new SSEParser();
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          for (const m of parser.push(decoder.decode(value, { stream: true }))) {
            if (m.id) lastId = m.id;
            h.onMessage(m);
            if (m.event === "end") return;
          }
        }
      } catch (err: any) {
        if (ac.signal.aborted) return;
        h.onStatus?.(false, err?.message ?? String(err));
      }
      attempt++;
      await new Promise((r) => setTimeout(r, Math.min(1000 * attempt, 10_000)));
    }
  })();
  return { stop: () => ac.abort(), done };
}
