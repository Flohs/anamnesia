import { cn } from "@/lib/cn";
import { artifactName } from "@/lib/artifact";

import { EmptyState } from "@/components/ui/EmptyState";
import { formatRelative } from "@/lib/format";
import type { Artifact } from "@/api/types";

interface ArtifactListProps {
  artifacts: readonly Artifact[];
  /** Index the ring is pointing at, so both views agree on one selection. */
  lit: number | null;
  onHover: (index: number | null) => void;
  /** Indices matching the running search; undefined when none is running. */
  matches?: readonly number[];
  /** Shared with the ring, so a project is one colour in both. */
  hueOf: (project: string | null | undefined) => string;
}

/**
 * The names beside the ring.
 *
 * The ring shows where an artifact sits and how the set is shaped; it cannot
 * show what any of them are called at tile size. This is the other half: one
 * scannable row each, sharing the ring's selection in both directions.
 */
export function ArtifactList({ artifacts, lit, onHover, matches, hueOf }: ArtifactListProps) {
  if (artifacts.length === 0) {
    return (
      <EmptyState title="No artifacts yet">
        An artifact is a page this memory published. They are captured as they are made, so this
        fills in as you work rather than needing a backfill.
      </EmptyState>
    );
  }

  const searching = matches !== undefined;

  return (
    <ul className="m-0 list-none p-0">
      {artifacts.map((artifact, index) => {
        const hit = searching && matches.includes(index);

        /**
         * A row that opens the artifact is a link, not a button: that is what
         * makes cmd-click, middle-click and "open in new tab" work, and what
         * a screen reader announces correctly. A row with no URL captured is
         * neither, rather than a link that goes nowhere.
         */
        const Row = artifact.url ? "a" : "div";
        const opens = artifact.url
          ? { href: artifact.url, target: "_blank", rel: "noreferrer noopener" }
          : { title: "No URL was captured for this artifact" };

        return (
          <li key={artifact.id}>
            <Row
              {...opens}
              data-row
              aria-current={lit === index ? "true" : undefined}
              data-hit={searching ? String(hit) : undefined}
              onMouseEnter={() => onHover(index)}
              onMouseLeave={() => onHover(null)}
              onFocus={() => onHover(index)}
              onBlur={() => onHover(null)}
              className={cn(
                "flex w-full items-center gap-3 border-0 border-b border-border no-underline",
                "bg-transparent px-4 py-2.5 text-left transition-colors",
                "focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-coral",
                artifact.url ? "cursor-pointer hover:bg-surface-2" : "cursor-default",
                lit === index && "bg-surface-2",
                searching && !hit && "opacity-30",
              )}
            >
              <span
                aria-hidden
                className="h-6.5 w-1 shrink-0 rounded-xs"
                style={{ background: hueOf(artifact.project) }}
              />
              <span className="min-w-0 flex-1">
                <span
                  className={cn(
                    "block truncate text-[13.5px] font-medium -tracking-[0.1px]",
                    lit === index || hit ? "text-text" : "text-text-2",
                  )}
                >
                  {artifactName(artifact)}
                </span>
                <span className="block data text-text-5">{artifact.project ?? "user-level"}</span>
              </span>
              <span className="shrink-0 data text-text-5">{formatRelative(artifact.created_at)}</span>
            </Row>
          </li>
        );
      })}
    </ul>
  );
}
