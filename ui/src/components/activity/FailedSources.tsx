import { EmptyState } from "@/components/ui/EmptyState";
import { Quote } from "@/components/ui/Quote";
import { useScopeNames } from "@/api/useScopeNames";
import { formatBytes, formatRelative, joinMeta } from "@/lib/format";
import type { Source } from "@/api/types";

/**
 * Sources the extractor could not turn into operations.
 *
 * The row's error is the whole point, so it leads. `raw_content` is present in
 * the response but deliberately not rendered: it is the entire conversation,
 * and this panel exists to say what went wrong, not to reproduce the session.
 */
export function FailedSources({ sources }: { sources: readonly Source[] }) {
  const names = useScopeNames();

  if (sources.length === 0) {
    return (
      <EmptyState title="Nothing has failed">
        A source fails when the extractor cannot turn it into operations, usually because the
        model returned something that is not JSON. The worker retries before giving up.
      </EmptyState>
    );
  }

  return (
    <ul className="m-0 list-none p-0">
      {sources.map((source) => (
        <li key={source.id} className="border-b border-border px-4 py-3.5 last:border-b-0">
          <div className="mb-2 flex flex-wrap items-baseline gap-x-3 gap-y-1">
            <span className="data text-text-5">{formatRelative(source.ingested_at)}</span>
            <span className="text-[14px] text-text-2">
              A {source.raw_content ? formatBytes(source.raw_content.length) : ""} {source.kind}{" "}
              from {names.project(source.scope)} could not be extracted
            </span>
          </div>

          {source.extraction_error ? (
            <Quote className="border-l-rose/50">{source.extraction_error}</Quote>
          ) : (
            <p className="m-0 text-[13px] text-text-4">
              The server recorded no reason, which usually means the worker was interrupted
              mid-run rather than the model failing.
            </p>
          )}

          <p className="mt-2 mb-0 data text-text-5">
            {joinMeta(
              source.id.slice(0, 8),
              source.extracted_at ? `last tried ${formatRelative(source.extracted_at)}` : "never retried",
              `raw content expires ${formatRelative(source.expires_at)}`,
            )}
          </p>
        </li>
      ))}
    </ul>
  );
}
