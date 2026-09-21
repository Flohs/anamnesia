import { cva, type VariantProps } from "class-variance-authority";
import type { ReactNode } from "react";

import { cn } from "@/lib/cn";

const alert = cva("flex items-start gap-3 rounded-md border px-4 py-3.5", {
  variants: {
    tone: {
      warn: "border-amber/25 bg-amber/10",
      bad: "border-rose/25 bg-rose/10",
      info: "border-sky/25 bg-sky/10",
    },
  },
  defaultVariants: { tone: "warn" },
});

const badge = cva("shrink-0 pt-0.5 data", {
  variants: {
    tone: { warn: "text-amber", bad: "text-rose", info: "text-sky" },
  },
  defaultVariants: { tone: "warn" },
});

interface AlertProps extends VariantProps<typeof alert> {
  /** Three or four uppercase letters naming the subsystem: ANN, LLM, NET. */
  code: string;
  title: string;
  children: ReactNode;
  className?: string;
}

/**
 * A diagnostic the reader can act on.
 *
 * The title states what is true, the body says why and names the command that
 * fixes it. No apologies, and never a bare error code.
 */
export function Alert({ code, title, children, tone = "warn", className }: AlertProps) {
  return (
    <div className={cn(alert({ tone }), className)} role="status">
      <span className={badge({ tone })}>{code}</span>
      <div className="min-w-0">
        <h3 className="text-[14.5px] font-semibold -tracking-[0.2px]">{title}</h3>
        <p className="mt-0.5 text-[13.5px] text-text-3">{children}</p>
      </div>
    </div>
  );
}

/** Inline command or setting name inside an Alert body. */
export function Code({ children }: { children: ReactNode }) {
  return (
    <code className="rounded-[4px] bg-black/30 px-1.5 py-px font-mono text-[12px] text-text-2">
      {children}
    </code>
  );
}
