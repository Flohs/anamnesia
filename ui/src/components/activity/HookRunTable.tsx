import { Chip } from "@/components/ui/Chip";
import { DataTable, type Column } from "@/components/ui/DataTable";
import { EmptyState } from "@/components/ui/EmptyState";
import { formatDuration, formatRelative } from "@/lib/format";
import type { HookRun } from "@/api/types";

const COLUMNS: ReadonlyArray<Column<HookRun>> = [
  { id: "at", header: "when", numeric: true, render: (row) => formatRelative(row.at) },
  {
    id: "verb",
    header: "hook",
    numeric: true,
    render: (row) => <span className="text-text-2">{row.verb}</span>,
  },
  {
    id: "ok",
    header: "result",
    render: (row) => (
      <Chip tone={row.ok ? "ok" : "bad"} withDot>
        {row.ok ? "ok" : "failed"}
      </Chip>
    ),
  },
  { id: "ms", header: "took", numeric: true, render: (row) => formatDuration(row.ms) },
  { id: "note", header: "note", render: (row) => row.note },
];

/** How many runs to show. The rest are counted, never silently dropped. */
const VISIBLE = 15;

/**
 * Hook runs come from hooks.log on disk, which makes them the only history
 * that survives a restart.
 *
 * The server returns the whole log and ignores `limit`, so the cap is applied
 * here. A long-lived install has thousands of lines and this panel is meant to
 * answer "did the last few sessions work", not to be a log viewer.
 */
export function HookRunTable({ runs }: { runs: readonly HookRun[] }) {
  const visible = runs.slice(0, VISIBLE);
  const hidden = runs.length - visible.length;

  return (
    <>
    <DataTable
      rows={visible}
      columns={COLUMNS}
      getRowKey={(row) => `${row.at}-${row.verb}`}
      caption="Recent Claude Code hook runs"
      empty={
        <EmptyState title="No hook has run yet">
          Anamnesia wires four hooks into Claude Code. Start a session and they will appear here,
          read from hooks.log.
        </EmptyState>
      }
    />
    {hidden > 0 ? (
      <p className="m-0 border-t border-border px-4 py-2.5 data text-text-5">
        {hidden} older {hidden === 1 ? "run" : "runs"} in the log, not shown
      </p>
    ) : null}
    </>
  );
}
