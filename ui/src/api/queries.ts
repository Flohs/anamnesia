import { useQuery, type UseQueryResult } from "@tanstack/react-query";

import { useTransport } from "@/api/TransportProvider";
import type { QueryParams, Transport } from "@/api/transport";
import type {
  ActivityBuckets,
  ActivitySnapshot,
  Artifact,
  Artifacts,
  Config,
  EmbeddingMap,
  Experience,
  Fact,
  Health,
  HookRuns,
  Page,
  Project,
  QueueDepths,
  Source,
  SourceState,
  Stats,
  Trace,
  User,
} from "@/api/types";

/**
 * Every read the console performs.
 *
 * One hook per endpoint, all going through the injected transport, so no
 * component ever calls fetch and a test can answer any of them without a
 * network.
 */

export const queryKeys = {
  health: ["health"] as const,
  activity: ["activity"] as const,
  trace: (id: string) => ["activity", id] as const,
  hooks: ["hooks"] as const,
  queues: ["queues"] as const,
  stats: (scope?: { user: string; project: string } | null) => ["stats", scope ?? "all"] as const,
  activityBuckets: (days: number) => ["stats", "activity", days] as const,
  projects: ["projects"] as const,
  users: ["users"] as const,
  experiences: (project: string | null) => ["experiences", project] as const,
  facts: (project: string | null) => ["facts", project] as const,
  sources: (state: string) => ["sources", state] as const,
  embeddingMap: (domain: string) => ["embedding-map", domain] as const,
  artifacts: ["artifacts"] as const,
  config: ["config"] as const,
};

/** Health is polled: it is the one thing worth knowing even when SSE is down. */
export function useHealth(): UseQueryResult<Health> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.health,
    queryFn: () => transport.get<Health>("/v1/health"),
    // The activity stream keeps delivering while the tab is hidden, so the
    // polled widgets must too. Otherwise a backgrounded console shows live
    // worker loops beside queue depths and health from minutes ago, and
    // nothing on screen says which is which. These payloads are tiny.
    refetchInterval: 15_000,
    refetchIntervalInBackground: true,
  });
}

/**
 * The activity snapshot. The stream also delivers one as its first frame, so
 * this is the fallback for when the stream cannot connect.
 */
export function useActivitySnapshot(enabled = true): UseQueryResult<ActivitySnapshot> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.activity,
    queryFn: () => transport.get<ActivitySnapshot>("/v1/activity"),
    enabled,
    refetchInterval: enabled ? 5_000 : false,
  });
}

export function useTrace(id: string | null): UseQueryResult<Trace> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.trace(id ?? ""),
    queryFn: () => transport.get<Trace>(`/v1/activity/${id ?? ""}`),
    enabled: id !== null,
  });
}

/**
 * Queue depth, polled.
 *
 * The activity stream is specified to emit a `queues` event and does not, so
 * these tiles would otherwise show the value from the moment the page
 * connected, for as long as it stayed open. `/v1/queue/pending` is a two-field
 * response, so polling it briskly costs nothing.
 */
export function useQueueDepths(): UseQueryResult<QueueDepths> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.queues,
    queryFn: () => transport.get<QueueDepths>("/v1/queue/pending"),
    refetchInterval: 5_000,
    refetchIntervalInBackground: true,
  });
}

export function useHookRuns(): UseQueryResult<HookRuns> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.hooks,
    queryFn: () => transport.get<HookRuns>("/v1/hooks"),
  });
}

export function useStats(scope?: { user: string; project: string } | null): UseQueryResult<Stats> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.stats(scope),
    queryFn: () => transport.get<Stats>("/v1/stats", scope ? { ...scope } : undefined),
    refetchInterval: 30_000,
    refetchIntervalInBackground: true,
  });
}

export function useActivityBuckets(days = 84): UseQueryResult<ActivityBuckets> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.activityBuckets(days),
    queryFn: () => transport.get<ActivityBuckets>("/v1/stats/activity", { days }),
  });
}

export function useProjects(): UseQueryResult<Page<Project>> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.projects,
    queryFn: () => transport.get<Page<Project>>("/v1/projects"),
    staleTime: 5 * 60_000,
  });
}

export function useUsers(): UseQueryResult<Page<User>> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.users,
    queryFn: () => transport.get<Page<User>>("/v1/users"),
    // The directory changes when a new repository is first seen, which is
    // rare, so this is fetched once and reused by every table.
    staleTime: 5 * 60_000,
  });
}

/**
 * Every artifact this memory has produced.
 *
 * The endpoint answers `{scope, artifacts}` rather than the `{items,
 * next_cursor}` shape every browse route uses, so this unwraps the envelope
 * instead of reaching for the paging helper. An install that has captured
 * nothing yields an empty list rather than undefined, so the ring can render
 * its own empty state without a null check at every call site.
 */
export function useArtifacts(): UseQueryResult<Artifact[]> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.artifacts,
    queryFn: async () => {
      const answer = await transport.get<Artifacts>("/v1/artifacts");
      return answer.artifacts ?? [];
    },
  });
}

export function useFacts(project: string | null = null): UseQueryResult<Page<Fact>> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.facts(project),
    queryFn: () => transport.get<Page<Fact>>("/v1/facts", project ? { project } : undefined),
  });
}

/**
 * Every experience, not the first page of them.
 *
 * The server answers a browse endpoint with 50 rows and a cursor. The figures
 * on the Shape view reason about the whole store rather than a screenful, and
 * a single page quietly makes the decay curve a recency-biased sample and
 * hides the far side of a fold whose source is older than the cutoff. Callers
 * that page for a reader want `useBrowse` instead.
 */
export function useExperiences(project: string | null = null): UseQueryResult<Page<Experience>> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.experiences(project),
    queryFn: () =>
      fetchEveryPage<Experience>(transport, "/v1/experiences", project ? { project } : undefined),
  });
}

/**
 * Pages an endpoint to the end, capped so that a store larger than anyone
 * expects degrades to a partial figure rather than an unbounded request loop.
 */
const MAX_PAGES = 20;

async function fetchEveryPage<T>(
  transport: Transport,
  path: string,
  params?: QueryParams,
): Promise<Page<T>> {
  const items: T[] = [];
  let cursor: string | undefined;

  for (let page = 0; page < MAX_PAGES; page += 1) {
    const result = await transport.get<Page<T>>(path, {
      ...params,
      ...(cursor ? { cursor } : {}),
    });

    items.push(...result.items);
    cursor = result.next_cursor ?? undefined;
    if (!cursor) break;
  }

  return { items, next_cursor: null };
}

/**
 * Sources in a given extraction state.
 *
 * Deliberately small limits: a source row carries its whole `raw_content`,
 * tens of kilobytes of conversation each, so this is never used to browse.
 */
export function useSources(state: SourceState, limit = 10): UseQueryResult<Page<Source>> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.sources(state),
    queryFn: () => transport.get<Page<Source>>("/v1/sources", { state, limit }),
  });
}

export function useEmbeddingMap(domain: "experiences" | "facts" = "experiences"): UseQueryResult<EmbeddingMap> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.embeddingMap(domain),
    queryFn: () => transport.get<EmbeddingMap>("/v1/embedding-map", { domain }),
  });
}

export function useConfig(): UseQueryResult<Config> {
  const { transport } = useTransport();
  return useQuery({
    queryKey: queryKeys.config,
    queryFn: () => transport.get<Config>("/v1/config"),
  });
}
