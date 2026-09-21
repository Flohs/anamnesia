import { cn } from "@/lib/cn";
import { ConnectionBadge } from "@/components/layout/ConnectionBadge";
import { VIEWS, type ViewId } from "@/views/registry";
import type { ConnectionState } from "@/api/types";

interface TopBarProps {
  view: ViewId;
  onNavigate: (view: ViewId) => void;
  connection: ConnectionState;
}

export function TopBar({ view, onNavigate, connection }: TopBarProps) {
  return (
    <header className="sticky top-0 z-40 border-b border-border bg-bg/85 backdrop-blur-md">
      <div className="mx-auto flex max-w-[1360px] flex-wrap items-center gap-x-7 gap-y-3 px-6 py-2.5 md:h-15 md:flex-nowrap md:py-0">
        <div className="flex items-center gap-2.5 text-[16.5px] font-bold -tracking-[0.4px]">
          <span className="size-[9px] rounded-full bg-coral shadow-glow" aria-hidden />
          Anamnesia
          <span className="rounded-sm border border-border-2 bg-surface px-1.5 py-0.5 data text-text-4">
            console
          </span>
        </div>

        <nav aria-label="Views" className="order-3 flex w-full gap-0.5 overflow-x-auto md:order-none md:w-auto">
          {VIEWS.map((entry) => (
            <button
              key={entry.id}
              type="button"
              aria-current={entry.id === view ? "page" : undefined}
              onClick={() => onNavigate(entry.id)}
              className={cn(
                "cursor-pointer rounded-lg border-0 bg-transparent px-3.5 py-1.5",
                "font-sans text-[14px] font-medium -tracking-[0.1px] transition-colors",
                entry.id === view
                  ? "bg-surface-2 text-text"
                  : "text-text-3 hover:bg-surface hover:text-text-2",
              )}
            >
              {entry.label}
            </button>
          ))}
        </nav>

        <div className="ml-auto flex items-center gap-2.5">
          <ConnectionBadge state={connection} />
        </div>
      </div>
    </header>
  );
}
