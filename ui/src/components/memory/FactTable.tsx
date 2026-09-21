import { DataTable, type Column } from "@/components/ui/DataTable";
import { EmptyState } from "@/components/ui/EmptyState";
import { ScoreBar } from "@/components/ui/ScoreBar";
import { useScopeNames, type ScopeNames } from "@/api/useScopeNames";
import { factValue } from "@/lib/fact";
import { formatRelative } from "@/lib/format";
import type { Fact } from "@/api/types";

const columns = (names: ScopeNames): ReadonlyArray<Column<Fact>> => [
  {
    id: "key",
    header: "key",
    render: (row) => <span className="font-mono text-[12.5px] text-text">{row.key}</span>,
  },
  {
    id: "value",
    header: "value",
    render: (row) => <span className="text-text-2">{factValue(row.value)}</span>,
  },
  { id: "scope", header: "scope", numeric: true, render: (row) => row.fact_scope },
  {
    id: "trust",
    header: "trust",
    render: (row) => <ScoreBar score={row.trust} tone="ok" />,
  },
  {
    id: "project",
    header: "project",
    numeric: true,
    render: (row) => names.project(row.scope),
  },
  {
    id: "ingested",
    header: "learned",
    numeric: true,
    render: (row) => formatRelative(row.ingested_at),
  },
];

export function FactTable({ facts }: { facts: readonly Fact[] }) {
  const names = useScopeNames();

  return (
    <DataTable
      rows={facts}
      columns={columns(names)}
      getRowKey={(row) => row.id}
      caption="Stored facts"
      empty={
        <EmptyState title="No facts yet">
          A fact is a claim the extractor thought worth keeping: a preference you stated, a
          decision you settled, a piece of project configuration. Most checkpoints are skipped as
          unsurprising, which is the gate working rather than a fault.
        </EmptyState>
      }
    />
  );
}
