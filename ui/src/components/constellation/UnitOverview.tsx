import { cn } from "@/lib/cn";
import { domainHue } from "@/components/constellation/hues";
import { groupThousands } from "@/lib/format";
import type { DomainTotals } from "@/api/types";

interface UnitOverviewProps {
  totals: DomainTotals;
  /** The domain lit in the die, or null when all of them are. */
  isolate: string | null;
  onIsolate: (domain: string | null) => void;
}

/**
 * Only three domains carry vectors, so only these can be lit in the die. The
 * rest are counted here but cannot be isolated, and say so by being disabled
 * rather than by doing nothing when clicked.
 */
const EMBEDDED = new Set(["facts", "experiences", "entities"]);

const UNITS = [
  "facts",
  "experiences",
  "entities",
  "edges",
  "sources",
  "commitments",
  "artifacts",
  "skills",
  "working_memory",
] as const;

/**
 * What the store holds, unit by unit.
 *
 * The ring answers "what has this memory made"; this answers "what is it made
 * of". A zero is kept rather than hidden, because an empty domain says which
 * worker has not run yet, which is worth knowing.
 */
export function UnitOverview({ totals, isolate, onIsolate }: UnitOverviewProps) {
  return (
    <div className="grid grid-cols-2 py-1">
      {UNITS.map((unit) => {
        const count = totals[unit] ?? 0;
        const canIsolate = EMBEDDED.has(unit);
        const on = isolate === unit;

        return (
          <button
            key={unit}
            type="button"
            disabled={!canIsolate}
            aria-pressed={canIsolate ? on : undefined}
            title={canIsolate ? undefined : "Not embedded, so it cannot be lit in the die"}
            onClick={() => onIsolate(on ? null : unit)}
            className={cn(
              "grid grid-cols-[9px_1fr_auto] items-center gap-2.5 px-4 py-1.5 text-left",
              "border-0 bg-transparent transition-colors",
              canIsolate && "cursor-pointer hover:bg-surface-2",
              on && "bg-surface-2",
              "focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-coral",
            )}
          >
            <span
              aria-hidden
              className="size-2.5 rounded-full transition-shadow"
              style={{
                background: domainHue(unit),
                opacity: count === 0 ? 0.3 : 1,
                boxShadow: on ? `0 0 9px ${domainHue(unit)}` : undefined,
              }}
            />
            <span
              className={cn(
                "truncate label",
                count === 0 ? "text-text-5" : on ? "text-text" : "text-text-3",
              )}
            >
              {unit.replace("_", " ")}
            </span>
            <span
              className={cn("data text-[13px]", count === 0 ? "text-text-5" : "text-text-2")}
            >
              {groupThousands(count)}
            </span>
          </button>
        );
      })}
    </div>
  );
}
