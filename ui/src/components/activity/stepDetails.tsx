import type { ComponentType } from "react";

import { KeyValue, type KeyValueRow } from "@/components/ui/KeyValue";
import { Quote } from "@/components/ui/Quote";
import { ScoreBar } from "@/components/ui/ScoreBar";
import { cn } from "@/lib/cn";
import { formatBytes, formatScore, groupThousands } from "@/lib/format";
import {
  readArray,
  readBoolean,
  readHits,
  readNumber,
  readString,
  readStringArray,
  remainingEntries,
  type Detail,
  type DetailHit,
} from "@/lib/detail";

/**
 * How each step shows its work.
 *
 * A registry keyed by step name, so adding a step to a pipeline means adding
 * one entry here rather than another branch in a growing switch. A step with
 * no entry still renders, through the generic fallback, which is what keeps
 * the console forward-compatible with a binary that gains steps.
 */

interface StepDetailProps {
  detail: Detail;
}

// ─── shared pieces ───────────────────────────────────────────────────

function HitList({ hits }: { hits: DetailHit[] }) {
  if (hits.length === 0) return null;

  return (
    <ul className="m-0 flex list-none flex-col gap-px overflow-hidden rounded border border-border bg-border p-0">
      {hits.map((hit) => (
        <li key={hit.id} className="grid grid-cols-[1fr_auto] items-center gap-3 bg-surface-2 px-3 py-2">
          <span className="truncate text-[13.5px] text-text-2">{hit.title}</span>
          {hit.score !== undefined ? <ScoreBar score={hit.score} /> : null}
        </li>
      ))}
    </ul>
  );
}

function Note({ children }: { children: string }) {
  return <p className="mt-2.5 mb-0 text-[12.5px] text-text-5">{children}</p>;
}

/** Anything the bespoke renderer did not show, so nothing is silently dropped. */
function Rest({ detail, shown }: { detail: Detail; shown: readonly string[] }) {
  const rows = remainingEntries(detail, shown).map(([label, value]) => ({ label, value }));
  return rows.length > 0 ? <KeyValue rows={rows} className="mt-2.5" /> : null;
}

// ─── per-step renderers ──────────────────────────────────────────────

function SourceDetail({ detail }: StepDetailProps) {
  const bytes = readNumber(detail, "bytes");
  const rows: KeyValueRow[] = [
    { label: "source", value: readString(detail, "source_id") ?? "" },
    { label: "kind", value: readString(detail, "kind") ?? "" },
    { label: "size", value: bytes !== undefined ? formatBytes(bytes) : "" },
    { label: "session", value: readString(detail, "session") ?? "" },
    { label: "offset", value: readString(detail, "byte_offset") ?? "" },
  ];
  const excerpt = readString(detail, "excerpt");

  return (
    <>
      <KeyValue rows={rows} />
      {excerpt ? (
        <Quote clamp={readBoolean(detail, "truncated") ?? false} className="mt-3">
          {excerpt}
        </Quote>
      ) : null}
    </>
  );
}

function GateDetail({ detail }: StepDetailProps) {
  const verdict = readString(detail, "verdict");
  const score = readNumber(detail, "score");
  const threshold = readNumber(detail, "threshold");

  return (
    <>
      <KeyValue
        rows={[
          {
            label: "verdict",
            value: verdict ? (
              <span className={verdict === "keep" ? "text-amber" : "text-text-4"}>{verdict}</span>
            ) : (
              ""
            ),
          },
          {
            label: "surprise",
            value:
              score !== undefined
                ? `${formatScore(score)}${threshold !== undefined ? ` · threshold ${formatScore(threshold)}` : ""}`
                : "",
          },
          { label: "reason", value: readString(detail, "reason") ?? "" },
        ]}
      />
      {verdict === "skip" ? (
        <Note>
          Skipping is the common case. The gate exists so unremarkable conversation never reaches
          the model.
        </Note>
      ) : null}
    </>
  );
}

function HitsDetail({ detail }: StepDetailProps) {
  const note = readString(detail, "note");
  return (
    <>
      <HitList hits={readHits(detail)} />
      {note ? <Note>{note}</Note> : null}
    </>
  );
}

function LlmDetail({ detail }: StepDetailProps) {
  const promptChars = readNumber(detail, "prompt_chars");
  const completionChars = readNumber(detail, "completion_chars");
  const memories = readNumber(detail, "memories_in_context");
  const retries = readNumber(detail, "retries");

  return (
    <KeyValue
      rows={[
        {
          label: "provider",
          value: [readString(detail, "provider"), readString(detail, "model")]
            .filter(Boolean)
            .join(" · "),
        },
        {
          label: "prompt",
          value:
            promptChars !== undefined
              ? `${groupThousands(promptChars)} chars${memories !== undefined ? ` · ${memories} memories in context` : ""}`
              : "",
        },
        {
          label: "completion",
          value: completionChars !== undefined ? `${groupThousands(completionChars)} chars` : "",
        },
        { label: "retries", value: retries !== undefined && retries > 0 ? String(retries) : "" },
      ]}
    />
  );
}

const OP_TONE: Record<string, string> = {
  ADD_FACT: "border-mint/28 bg-mint/8 text-mint",
  UPDATE_FACT: "border-sky/28 bg-sky/10 text-sky",
  DELETE_FACT: "border-rose/28 bg-rose/10 text-rose",
  ADD_EXPERIENCE: "border-sky/28 bg-sky/10 text-sky",
  NOOP: "border-border-2 bg-surface-2 text-text-4",
};

function OpsDetail({ detail }: StepDetailProps) {
  const operations = readArray(detail, "operations");
  if (operations.length === 0) return null;

  return (
    <div className="flex flex-col gap-2.5">
      {operations.map((entry, index) => {
        const op = (typeof entry === "object" && entry !== null ? entry : {}) as Detail;
        const name = readString(op, "op") ?? "OPERATION";
        const body = readString(op, "value") ?? readString(op, "body") ?? "";
        const heading = readString(op, "key") ?? readString(op, "title");
        const meta = [
          readString(op, "scope"),
          readString(op, "kind"),
          readNumber(op, "abstraction") !== undefined
            ? `abstraction ${readNumber(op, "abstraction")}`
            : undefined,
          readNumber(op, "trust") !== undefined
            ? `trust ${formatScore(readNumber(op, "trust") as number)}`
            : undefined,
          readNumber(op, "importance") !== undefined
            ? `importance ${formatScore(readNumber(op, "importance") as number)}`
            : undefined,
        ]
          .filter(Boolean)
          .join(" · ");

        return (
          <article key={index} className="rounded border border-border-2 bg-bg-2 px-3.5 py-3">
            <header className="mb-2 flex flex-wrap items-center gap-2.5">
              <span
                className={cn(
                  "rounded-sm border px-1.5 py-0.5 label",
                  OP_TONE[name] ?? OP_TONE["NOOP"],
                )}
              >
                {name}
              </span>
              {meta ? <span className="data text-text-4">{meta}</span> : null}
            </header>

            {heading ? (
              <p className="m-0 mb-1 font-mono text-[12px] text-text-2">{heading}</p>
            ) : null}
            {body ? (
              <p className="m-0 whitespace-pre-wrap text-[13px] text-text-3">{body}</p>
            ) : null}
          </article>
        );
      })}
    </div>
  );
}

function WrittenDetail({ detail }: StepDetailProps) {
  const written = readArray(detail, "written");
  const rows: KeyValueRow[] = written.flatMap((entry) => {
    const row = (typeof entry === "object" && entry !== null ? entry : {}) as Detail;
    const target = readString(row, "target");
    if (!target) return [];
    return [
      {
        label: target,
        value: [readString(row, "id")?.slice(0, 8), readString(row, "label")]
          .filter(Boolean)
          .join(" · "),
      },
    ];
  });

  return (
    <>
      <KeyValue rows={rows} />
      <Rest detail={detail} shown={["written", "errors"]} />
    </>
  );
}

function RerankDetail({ detail }: StepDetailProps) {
  const before = readStringArray(detail, "before");
  const after = readStringArray(detail, "after");

  if (before.length === 0 || after.length === 0) {
    return <GenericDetail detail={detail} />;
  }

  return (
    <>
      <div className="grid items-center gap-2 md:grid-cols-[1fr_46px_1fr]">
        <RankColumn titles={before} />
        <div className="flex flex-row items-center justify-center gap-1.5 data text-text-5 md:flex-col md:gap-1">
          <span>rrf</span>
          <span aria-hidden className="text-[14px]">
            &rarr;
          </span>
          <span>rerank</span>
        </div>
        <RankColumn titles={after} movedFrom={before} />
      </div>
      <Note>
        Reranking is the difference between the right answer and a plausible neighbour of it.
      </Note>
    </>
  );
}

function RankColumn({ titles, movedFrom }: { titles: string[]; movedFrom?: string[] }) {
  return (
    <ol className="m-0 flex list-none flex-col gap-1.5 p-0">
      {titles.map((title, index) => {
        const previous = movedFrom?.indexOf(title);
        const promoted = previous !== undefined && previous > index;

        return (
          <li
            key={title}
            className={cn(
              "flex items-center gap-2.5 rounded-sm border bg-surface-2 px-2.5 py-1.5 text-[12.5px]",
              promoted ? "border-mint/35 text-text" : "border-border-2 text-text-2",
            )}
          >
            <span className={cn("w-3.5 shrink-0 data", promoted ? "text-mint" : "text-text-5")}>
              {index + 1}
            </span>
            <span className="truncate">{title}</span>
          </li>
        );
      })}
    </ol>
  );
}

function ClusterDetail({ detail }: StepDetailProps) {
  const clusters = readArray(detail, "clusters");
  if (clusters.length === 0) return <GenericDetail detail={detail} />;

  return (
    <div className="flex flex-col gap-2.5">
      {clusters.map((entry, index) => {
        const cluster = (typeof entry === "object" && entry !== null ? entry : {}) as Detail;
        const members = readStringArray(cluster, "members");
        const similarity = readNumber(cluster, "centroid_similarity");

        return (
          <article key={index} className="rounded border border-border-2 bg-bg-2 px-3.5 py-3">
            <header className="mb-1.5 flex items-center gap-2.5">
              <span className="label text-lilac">cluster {index + 1}</span>
              {similarity !== undefined ? (
                <span className="data text-text-4">similarity {formatScore(similarity)}</span>
              ) : null}
            </header>
            <ul className="m-0 list-none p-0">
              {members.map((member) => (
                <li key={member} className="truncate text-[13px] text-text-3">
                  {member}
                </li>
              ))}
            </ul>
          </article>
        );
      })}
    </div>
  );
}

function DistilDetail({ detail }: StepDetailProps) {
  const title = readString(detail, "result_title");
  const body = readString(detail, "result_body");

  return (
    <>
      <KeyValue
        rows={[
          { label: "model", value: readString(detail, "model") ?? "" },
          { label: "cluster", value: readNumber(detail, "cluster_index") ?? "" },
        ]}
      />
      {title ? (
        <article className="mt-3 rounded border border-border-2 bg-bg-2 px-3.5 py-3">
          <h4 className="m-0 mb-1 text-[14px] font-semibold -tracking-[0.2px]">{title}</h4>
          {body ? <p className="m-0 text-[13px] text-text-3">{body}</p> : null}
        </article>
      ) : null}
    </>
  );
}

/** Everything printable, for steps without a bespoke renderer. */
function GenericDetail({ detail }: StepDetailProps) {
  const hits = readHits(detail);
  const rows = remainingEntries(detail, []).map(([label, value]) => ({ label, value }));

  return (
    <>
      <KeyValue rows={rows} />
      {hits.length > 0 ? <HitList hits={hits} /> : null}
    </>
  );
}

// ─── the registry ────────────────────────────────────────────────────

const RENDERERS: Record<string, ComponentType<StepDetailProps>> = {
  source: SourceDetail,
  gate: GateDetail,
  similar: HitsDetail,
  vector: HitsDetail,
  lexical: HitsDetail,
  llm: LlmDetail,
  ops: OpsDetail,
  apply: WrittenDetail,
  write: WrittenDetail,
  rerank: RerankDetail,
  cluster: ClusterDetail,
  distil: DistilDetail,
};

export function StepDetail({ name, detail }: { name: string; detail: Detail }) {
  const Renderer = RENDERERS[name] ?? GenericDetail;
  return <Renderer detail={detail} />;
}
