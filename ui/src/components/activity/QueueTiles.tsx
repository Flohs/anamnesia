import { Panel } from "@/components/ui/Panel";
import { cn } from "@/lib/cn";
import type { Tone } from "@/lib/tone";
import { TONE_TEXT } from "@/lib/tone";

export interface QueueTile {
  value: number;
  label: string;
  /** Only set when a non-zero value means something is wrong. */
  toneWhenNonZero?: Tone;
}

/**
 * Counts that answer "is anything stuck".
 *
 * Zero is the good answer for most of these, so a zero is dimmed and a
 * non-zero only turns a colour where a backlog actually matters.
 */
export function QueueTiles({ tiles }: { tiles: readonly QueueTile[] }) {
  return (
    <Panel className="overflow-hidden">
      <div className="grid grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-px bg-border">
        {tiles.map((tile) => (
          <div key={tile.label} className="bg-surface px-4 py-4">
            <div
              className={cn(
                "text-[27px] font-bold leading-tight -tracking-[1px] tabular-nums",
                tile.value === 0
                  ? "text-text-5"
                  : tile.toneWhenNonZero
                    ? TONE_TEXT[tile.toneWhenNonZero]
                    : "text-text",
              )}
            >
              {tile.value}
            </div>
            <div className="mt-0.5 label text-text-5">{tile.label}</div>
          </div>
        ))}
      </div>
    </Panel>
  );
}
