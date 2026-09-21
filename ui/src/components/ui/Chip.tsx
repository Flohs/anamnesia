import { cva, type VariantProps } from "class-variance-authority";
import type { ReactNode } from "react";

import { cn } from "@/lib/cn";
import { StatusDot } from "@/components/ui/StatusDot";

/**
 * The small monospace pill used for states, project names and counts.
 *
 * Tone classes are written out per variant rather than composed from the tone
 * maps, because Tailwind only emits classes it can find as literal text.
 */
const chip = cva(
  "inline-flex items-center gap-1.5 whitespace-nowrap rounded-sm border px-2 py-0.5 label",
  {
    variants: {
      tone: {
        neutral: "border-border-2 bg-surface-2 text-text-4",
        ok: "border-mint/28 bg-mint/8 text-mint",
        warn: "border-amber/28 bg-amber/10 text-amber",
        bad: "border-rose/28 bg-rose/10 text-rose",
        run: "border-coral/30 bg-coral/12 text-coral",
        info: "border-sky/28 bg-sky/10 text-sky",
        special: "border-lilac/28 bg-lilac/10 text-lilac",
      },
    },
    defaultVariants: { tone: "neutral" },
  },
);

interface ChipProps extends VariantProps<typeof chip> {
  children: ReactNode;
  /** Shows a matching dot, for states where the colour carries meaning. */
  withDot?: boolean;
  className?: string;
}

export function Chip({ children, tone = "neutral", withDot = false, className }: ChipProps) {
  return (
    <span className={cn(chip({ tone }), className)}>
      {withDot ? <StatusDot tone={tone ?? "neutral"} /> : null}
      {children}
    </span>
  );
}
