import { cn } from "@/lib/cn";
import { TONE_DOT, type Tone } from "@/lib/tone";

interface StatusDotProps {
  tone: Tone;
  /** Pulses while something is genuinely in flight, never for decoration. */
  pulse?: boolean | undefined;
  className?: string | undefined;
  /** Screen-reader text, since colour alone is not a status. */
  label?: string | undefined;
}

export function StatusDot({ tone, pulse = false, className, label }: StatusDotProps) {
  return (
    <span
      className={cn(
        "size-[7px] shrink-0 rounded-full",
        TONE_DOT[tone],
        pulse && "animate-[dot-pulse_1.6s_ease-in-out_infinite]",
        className,
      )}
      role={label ? "img" : "presentation"}
      aria-label={label}
    />
  );
}
