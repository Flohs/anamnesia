import { cn } from "@/lib/cn";

interface QuoteProps {
  children: string;
  /** Fades out at a fixed height, for content the server truncated. */
  clamp?: boolean;
  className?: string;
}

/**
 * Raw material quoted verbatim: a checkpoint's text, an error from the
 * extractor. Fades rather than cutting mid-word, so truncation reads as
 * deliberate.
 */
export function Quote({ children, clamp = false, className }: QuoteProps) {
  return (
    <div
      className={cn(
        "relative whitespace-pre-wrap border-l-2 border-border-bright py-0.5 pl-3.5",
        "text-[13.5px] text-text-3",
        clamp && "max-h-[120px] overflow-hidden",
        clamp &&
          "after:pointer-events-none after:absolute after:inset-x-0 after:bottom-0 after:h-8 after:bg-gradient-to-b after:from-transparent after:to-surface",
        className,
      )}
    >
      {children}
    </div>
  );
}
