import { cn } from "@/lib/cn";

interface LoadMoreProps {
  hasMore: boolean;
  loading: boolean;
  onClick: () => void;
  /** How many rows are already shown, so the count is never a mystery. */
  shown: number;
}

/**
 * The end of a list.
 *
 * Says how many rows are shown either way. A list that silently stops at 50
 * looks identical to a list that holds exactly 50, and that ambiguity is the
 * thing worth removing.
 */
export function LoadMore({ hasMore, loading, onClick, shown }: LoadMoreProps) {
  if (!hasMore) {
    return shown > 0 ? (
      <p className="m-0 border-t border-border px-4 py-2.5 data text-text-5">
        {shown} {shown === 1 ? "row" : "rows"}, all of them
      </p>
    ) : null;
  }

  return (
    <div className="flex items-center gap-3 border-t border-border px-4 py-2.5">
      <button
        type="button"
        onClick={onClick}
        disabled={loading}
        className={cn(
          "cursor-pointer rounded border border-border-2 bg-surface-2 px-2.5 py-1",
          "label text-text-3 hover:border-border-bright hover:text-text-2",
          loading && "cursor-wait opacity-60",
        )}
      >
        {loading ? "loading" : "load more"}
      </button>
      <span className="data text-text-5">{shown} shown</span>
    </div>
  );
}
