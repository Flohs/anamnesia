import type { ReactNode } from "react";

interface PageHeaderProps {
  /** The plain part of the headline. */
  children: ReactNode;
  /** The one word set in italic serif, as on anamnesia.dev. */
  accent: string;
  /** Punctuation after the accent word, usually a full stop. */
  tail?: string;
  lede: string;
}

/**
 * The headline for a view.
 *
 * anamnesia.dev sets exactly one word of each headline in italic Instrument
 * Serif. Enforcing that here keeps it a signature rather than a decoration
 * applied by feel.
 */
export function PageHeader({ children, accent, tail = ".", lede }: PageHeaderProps) {
  return (
    <header>
      <h1 className="mb-1.5 text-balance text-[34px] font-bold -tracking-[1.2px]">
        {children}{" "}
        <em className="font-serif text-[1.05em] font-normal not-italic italic -tracking-[0.4px] text-coral">
          {accent}
        </em>
        {tail}
      </h1>
      <p className="mb-6 max-w-[64ch] text-text-3">{lede}</p>
    </header>
  );
}
