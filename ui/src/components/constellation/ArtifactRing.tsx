import { useCallback, useEffect, useRef, useState } from "react";

import { cn } from "@/lib/cn";
import { artifactName } from "@/lib/artifact";
import { EmptyState } from "@/components/ui/EmptyState";
import {
  MemoryDie,
  type CloudPoint,
} from "@/components/constellation/MemoryDie";
import type { Artifact } from "@/api/types";

interface ArtifactRingProps {
  artifacts: readonly Artifact[];
  cloud: readonly CloudPoint[];
  /** Index the list is pointing at, so both views agree on one selection. */
  lit: number | null;
  onHover: (index: number | null) => void;
  hueOf: (project: string | null | undefined) => string;
  isolate?: string | null;
  /** Told which artifacts match, so the list can dim in step with the ring. */
  onMatches?: (matches: number[] | undefined) => void;
}

/** The ring opens on the right so the "i" has a berth rather than a tile. */
const GAP = 0.4;
const TAU = Math.PI * 2;

/**
 * Every artifact this memory has made, in orbit around the store it came from.
 *
 * The ring shows the shape of the set and where any one of them sits; it
 * cannot show what they are called at tile size, which is what the "i" is
 * for. Hovering it flies every name in at once, clicking it opens a search,
 * and both the ring and the list beside it light the same matches.
 */
export function ArtifactRing({
  artifacts,
  cloud,
  lit,
  onHover,
  hueOf,
  isolate = null,
  onMatches,
}: ArtifactRingProps) {
  const [namesShown, setNamesShown] = useState(false);
  const [searching, setSearching] = useState(false);
  const [query, setQuery] = useState("");
  const inputRef = useRef<HTMLInputElement | null>(null);

  const term = query.trim().toLowerCase();
  const matches = artifacts.reduce<number[]>((found, artifact, index) => {
    const hay = `${artifactName(artifact)} ${artifact.project ?? ""} ${artifact.description ?? ""}`;
    if (term !== "" && hay.toLowerCase().includes(term)) found.push(index);
    return found;
  }, []);

  const active = searching && term !== "";

  useEffect(() => {
    onMatches?.(active ? matches : undefined);
    // The identity of `matches` changes every render; its contents are what
    // the list cares about.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active, matches.join(","), onMatches]);

  const close = useCallback(() => {
    setSearching(false);
    setQuery("");
  }, []);

  useEffect(() => {
    if (!searching) return;
    inputRef.current?.focus();
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") close();
    };
    addEventListener("keydown", onKey);
    return () => removeEventListener("keydown", onKey);
  }, [searching, close]);

  if (artifacts.length === 0) {
    return (
      <EmptyState title="No artifacts yet">
        An artifact is a page this memory published. They are captured as they
        are made, so the ring fills in as you work rather than needing a
        backfill.
      </EmptyState>
    );
  }

  return (
    <div className="relative size-full">
      <MemoryDie cloud={cloud} isolate={isolate} />

      {/* The ring is laid out in percentages, so it needs a square to be a
          circle rather than an ellipse. Bound by the shorter side, centred,
          which is the same box the die draws itself into. */}
      <div className="absolute inset-0 grid place-items-center">
        <div className="relative aspect-square h-full max-w-full">
          {artifacts.map((artifact, index) => {
            const angle = GAP / 2 + (index / artifacts.length) * (TAU - GAP);
            const hue = hueOf(artifact.project);
            const hit = active && matches.includes(index);
            const shown = namesShown || hit || lit === index;
            const name = artifactName(artifact);
            const toRight = Math.cos(angle) >= 0;

            // A tile that opens the artifact is a link, for the same reason a
            // row is: cmd-click and middle-click are the browser's job, not
            // ours. A tile with no URL captured stays inert rather than
            // looking clickable and doing nothing.
            const Tile = artifact.url ? "a" : "div";
            const opens = artifact.url
              ? { href: artifact.url, target: "_blank", rel: "noreferrer noopener" }
              : {};

            return (
              <div key={artifact.id}>
                <Tile
                  {...opens}
                  aria-current={lit === index ? "true" : undefined}
                  aria-label={`${name}, ${artifact.project ?? "user-level"}`}
                  data-hit={active ? String(hit) : undefined}
                  onMouseEnter={() => onHover(index)}
                  onMouseLeave={() => onHover(null)}
                  onFocus={() => onHover(index)}
                  onBlur={() => onHover(null)}
                  style={{
                    left: `${50 + Math.cos(angle) * 39}%`,
                    top: `${50 + Math.sin(angle) * 39}%`,
                  }}
                  className={cn(
                    "absolute -ml-[18px] -mt-[18px] size-9 rounded-full border p-0",
                    "grid place-items-center transition-[transform,box-shadow,opacity]",
                    "bg-[radial-gradient(circle_at_50%_34%,#17171f,#0b0b10)]",
                    "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-coral",
                    artifact.url && "cursor-pointer",
                    (lit === index || hit) && "scale-110",
                    active && !hit && "opacity-15",
                  )}
                >
                  <span
                    aria-hidden
                    className="absolute inset-0 rounded-full border"
                    style={{
                      borderColor: lit === index || hit ? hue : `${hue}66`,
                      boxShadow:
                        lit === index || hit ? `0 0 16px ${hue}99` : undefined,
                    }}
                  />
                  <svg
                    viewBox="0 0 16 16"
                    aria-hidden
                    className="size-[15px] fill-none stroke-text-3"
                  >
                    <path d="M3 4h10v8H3z" strokeWidth="1.5" />
                    <path d="M3 6.4h10" strokeWidth="1.5" />
                    <path d="M5.1 4v2.4" strokeWidth="1.5" />
                  </svg>
                  <span className="absolute -left-1.5 -top-1 rounded-xs border border-border-2 bg-bg px-1 data text-text-4">
                    {index + 1}
                  </span>
                </Tile>

                <span
                  data-name
                  data-shown={String(shown)}
                  aria-hidden
                  style={{
                    left: toRight
                      ? `calc(${50 + Math.cos(angle) * 39}% + 26px)`
                      : "auto",
                    right: toRight
                      ? "auto"
                      : `calc(${50 - Math.cos(angle) * 39}% + 26px)`,
                    top: `calc(${50 + Math.sin(angle) * 39}% - 9px)`,
                    transitionDelay: `${index * 9}ms`,
                  }}
                  className={cn(
                    "pointer-events-none absolute z-5 whitespace-nowrap rounded-xs border border-border-2",
                    "bg-surface px-1.5 py-0.5 text-[10.5px] -tracking-[0.1px] text-text-2",
                    "transition-opacity duration-200",
                    shown ? "opacity-100" : "opacity-0",
                  )}
                >
                  <span className="mr-1 data text-coral">{index + 1}</span>
                  {name.length > 22 ? `${name.slice(0, 21)}…` : name}
                </span>
              </div>
            );
          })}

          <button
            type="button"
            aria-label="Show every artifact name, or search them"
            aria-expanded={searching}
            onMouseEnter={() => !searching && setNamesShown(true)}
            onMouseLeave={() => !searching && setNamesShown(false)}
            onClick={() => {
              setNamesShown(false);
              setSearching((was) => !was);
              setQuery("");
            }}
            style={{ left: "89%", top: "50%" }}
            className={cn(
              "absolute -ml-[19px] -mt-[19px] grid size-9.5 cursor-pointer place-items-center",
              "rounded-full border border-coral/55 font-mono text-[15px] font-medium text-coral",
              "bg-[radial-gradient(circle_at_50%_35%,#48210f,#23100a)] shadow-[0_0_18px_rgb(255_122_69/0.3)]",
              "transition-[transform,box-shadow] hover:scale-108 hover:shadow-[0_0_30px_rgb(255_122_69/0.68)]",
              "focus-visible:outline-2 focus-visible:outline-offset-3 focus-visible:outline-coral",
            )}
          >
            i
          </button>
        </div>
      </div>

      {searching ? (
        <div className="pointer-events-none absolute inset-0 z-10 grid place-items-center">
          <div className="pointer-events-auto w-[min(330px,82%)] rounded-md border border-coral bg-[rgb(16_12_10/0.94)] px-4 pb-2.5 pt-3 text-center shadow-[0_0_34px_rgb(255_122_69/0.22)]">
            <input
              ref={inputRef}
              type="search"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder="type an artifact name"
              autoComplete="off"
              spellCheck={false}
              aria-label="Search artifacts"
              className={cn(
                "w-full border-0 bg-transparent text-center text-[16px] font-semibold uppercase",
                "tracking-[0.8px] text-text outline-none",
                "placeholder:text-[12.5px] placeholder:font-normal placeholder:normal-case",
                "placeholder:tracking-[0.4px] placeholder:text-text-5",
                "[&::-webkit-search-cancel-button]:hidden",
              )}
            />
            <p className="mt-1 label text-coral">
              {term === ""
                ? `${artifacts.length} artifacts`
                : `${matches.length} ${matches.length === 1 ? "match" : "matches"}`}
            </p>
            <p className="mt-1.5 data text-text-5">ESC to close</p>
          </div>
        </div>
      ) : null}
    </div>
  );
}
