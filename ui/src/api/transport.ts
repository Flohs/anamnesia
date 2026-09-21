import type { ActivityEvent } from "@/api/types";

/**
 * Everything the console needs from the outside world.
 *
 * Components never fetch: they call the query hooks, the hooks call a
 * Transport, and only this module knows about HTTP. Tests inject a stub
 * through TransportProvider, which is what keeps the network out of them.
 */
export interface Transport {
  readonly name: "http";

  get<T>(path: string, params?: QueryParams): Promise<T>;

  /**
   * Subscribes to the activity stream. Returns an unsubscribe function.
   * `onError` fires when the connection drops so the UI can say it is polling
   * rather than silently going stale.
   */
  subscribe(handlers: StreamHandlers): Unsubscribe;
}

export type QueryParams = Record<string, string | number | boolean | undefined>;

export type Unsubscribe = () => void;

export interface StreamHandlers {
  onEvent: (event: ActivityEvent) => void;
  onOpen?: () => void;
  onError?: (error: Error) => void;
}

/** A failed request, carrying the status so callers can tell 401 from 503. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly path: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export function buildUrl(base: string, path: string, params?: QueryParams): string {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(params ?? {})) {
    if (value !== undefined) query.set(key, String(value));
  }
  const qs = query.toString();
  return `${base}${path}${qs ? `?${qs}` : ""}`;
}

/**
 * Talks to `anamnesia serve` through the same-origin `/api` prefix, which the
 * Vite dev server and the production Fastify server both proxy. The server
 * token is attached by that proxy, never here, so it stays out of the browser.
 */
export function createHttpTransport(base = "/api"): Transport {
  return {
    name: "http",

    async get<T>(path: string, params?: QueryParams): Promise<T> {
      const url = buildUrl(base, path, params);
      let response: Response;

      try {
        response = await fetch(url, { headers: { Accept: "application/json" } });
      } catch {
        throw new ApiError(
          `Cannot reach anamnesia at ${url}. Is \`anamnesia serve\` running, and bound where this container can see it?`,
          0,
          path,
        );
      }

      if (!response.ok) {
        throw new ApiError(await describeFailure(response), response.status, path);
      }
      return (await response.json()) as T;
    },

    subscribe({ onEvent, onOpen, onError }: StreamHandlers): Unsubscribe {
      const source = new EventSource(`${base}/v1/activity/stream`);

      // Each event type arrives under its own SSE event name, so the payload
      // is parsed once and handed on already discriminated.
      const kinds: ActivityEvent["type"][] = ["snapshot", "trace", "step", "loops", "queues"];
      for (const kind of kinds) {
        source.addEventListener(kind, (message) => {
          try {
            const data = JSON.parse((message as MessageEvent<string>).data) as never;
            onEvent({ type: kind, data });
          } catch {
            onError?.(new Error(`Malformed ${kind} event from the activity stream`));
          }
        });
      }

      source.addEventListener("open", () => onOpen?.());
      source.addEventListener("error", () => {
        onError?.(new Error("The activity stream disconnected"));
      });

      return () => source.close();
    },
  };
}

/**
 * Turns a failed response into a sentence that names the fix, because a bare
 * status code in a console is a dead end for whoever is reading it.
 */
async function describeFailure(response: Response): Promise<string> {
  const body = (await response.text().catch(() => "")).trim();

  if (response.status === 401) {
    return "anamnesia rejected the request. The server has a `server.token` set and the console was started without a matching ANAMNESIA_TOKEN.";
  }
  if (response.status === 404) {
    return "This endpoint is not served by the running anamnesia. It may predate the read API, or `activity.traces` may be set to 0.";
  }
  if (response.status === 503) {
    return body || "anamnesia is up but unhealthy. Check the database and the schema version.";
  }
  return body || `anamnesia returned ${response.status}.`;
}
