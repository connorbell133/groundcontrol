import { journal } from "./workspace.js";

// One notification mechanism: lifecycle events fan out to an in-process bus
// (SSE, wait=ready) and to configured webhook subscribers. ntfy, n8n, or
// anything that accepts a JSON POST catches them — no receiver is special.
export interface LifecycleEvent {
  event: string;
  at: string;
  title: string;
  message: string;
  data: Record<string, unknown>;
}

export interface WebhookConfig {
  url: string;
  events?: string[]; // match tokens; omitted or ["*"] = everything
}

let webhooks: WebhookConfig[] = [];

export function configureWebhooks(cfg?: WebhookConfig[]) {
  webhooks = cfg ?? [];
}

type Listener = (e: LifecycleEvent) => void;
const listeners = new Set<Listener>();

export function onEvent(fn: Listener): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

// A subscriber's filter matches the event name, a prefix wildcard ("session.*"),
// "*", or a derived token (a failed exit also matches "session.failed"/"job.failed").
function matches(filter: string[] | undefined, tokens: string[]): boolean {
  if (!filter || filter.length === 0) return true;
  return filter.some((f) => f === "*" || tokens.includes(f) || (f.endsWith(".*") && tokens.some((t) => t.startsWith(f.slice(0, -1)))));
}

export function emit(
  event: string,
  data: Record<string, unknown>,
  opts: { title?: string; message?: string; alsoMatch?: string[] } = {}
) {
  const e: LifecycleEvent = {
    event,
    at: new Date().toISOString(),
    title: opts.title ?? event,
    message: opts.message ?? "",
    data,
  };
  for (const l of [...listeners]) {
    try {
      l(e);
    } catch {
      /* one bad subscriber must not break the others */
    }
  }
  const tokens = [event, ...(opts.alsoMatch ?? [])];
  for (const hook of webhooks) {
    if (matches(hook.events, tokens)) deliverWebhook(hook.url, e);
  }
}

// Fire-and-forget POST: same contract as the old ntfy path — bounded, and a
// hanging endpoint must never delay lifecycle handling. Failures are journaled
// so silent drops stay diagnosable.
export function deliverWebhook(url: string, payload: LifecycleEvent) {
  try {
    fetch(url, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(payload),
      signal: AbortSignal.timeout(5000),
    })
      .then((res) => {
        if (!res.ok) journal({ event: "webhook.failed", url, status: res.status, for: payload.event });
      })
      .catch((err: Error) => {
        journal({ event: "webhook.failed", url, reason: err.message, for: payload.event });
      });
  } catch {
    /* invalid URL slipped through validation — nothing else to do */
  }
}
