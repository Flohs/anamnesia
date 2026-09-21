import type { Health } from "@/api/types";
import { StatusDot } from "@/components/ui/StatusDot";
import { shortModel } from "@/lib/trace";
import { formatUptime, joinMeta } from "@/lib/format";
import type { Tone } from "@/lib/tone";

interface RibbonCell {
  label: string;
  value: string;
  detail: string;
  tone?: Tone;
}

interface StatusRibbonProps {
  health: Health;
  /**
   * When the server started. Uptime is derived from this at render rather
   * than taken from the snapshot's `uptime_ms`, which is measured once when
   * the stream connects and then never moves.
   */
  startedAt: string | null;
  projectCount: number;
  experienceCount: number;
  factCount: number;
  entityCount: number;
  user: string;
}

/**
 * The six things worth knowing before anything else.
 *
 * Derived rather than decorative: the embeddings cell turns amber precisely
 * when an ANN index is missing, because that is the difference between a
 * vector search and a full table scan.
 */
function buildRibbon(props: StatusRibbonProps): RibbonCell[] {
  const { health, startedAt, projectCount, experienceCount, factCount, entityCount, user } = props;
  const annMissing = (health.missing_ann_indexes ?? []).length > 0;

  return [
    {
      label: "server",
      value: health.ok ? "up" : "unhealthy",
      detail: joinMeta(health.version, startedAt ? `${formatUptime(Date.now() - new Date(startedAt).getTime())} uptime` : null),
      tone: health.ok ? "ok" : "bad",
    },
    {
      label: "database",
      value: health.database,
      detail: joinMeta("postgres", `schema v${health.migration_version}`),
      tone: health.database === "ok" ? "ok" : "bad",
    },
    {
      label: "model",
      value: health.llm_model ? shortModel(health.llm_model) : "none",
      detail: health.llm_provider ?? "no provider configured",
      tone: health.llm_provider && health.llm_provider !== "stub" ? "ok" : "bad",
    },
    {
      label: "embeddings",
      value: health.embed_model ? shortModel(health.embed_model) : "none",
      detail: joinMeta(
        `${health.schema_embed_dims} dims`,
        annMissing ? "no ANN index" : "indexed",
      ),
      tone: annMissing ? "warn" : "ok",
    },
    { label: "scope", value: user, detail: `${projectCount} projects` },
    {
      label: "stored",
      value: `${experienceCount} experiences`,
      detail: joinMeta(`${factCount} facts`, `${entityCount} entities`),
    },
  ];
}

export function StatusRibbon(props: StatusRibbonProps) {
  return (
    <div className="grid grid-cols-[repeat(auto-fit,minmax(168px,1fr))] overflow-hidden rounded-md border border-border bg-surface">
      {buildRibbon(props).map((cell) => (
        <div
          key={cell.label}
          className="flex min-w-0 flex-col gap-0.5 border-r border-border px-4 py-3 last:border-r-0"
        >
          <span className="label text-text-5">{cell.label}</span>
          <span className="flex items-center gap-2 truncate text-[14.5px] font-semibold -tracking-[0.2px]">
            {cell.tone ? <StatusDot tone={cell.tone} /> : null}
            {cell.value}
          </span>
          <span className="truncate data text-text-4">{cell.detail}</span>
        </div>
      ))}
    </div>
  );
}
