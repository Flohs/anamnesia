import { DataTable, type Column } from "@/components/ui/DataTable";
import { formatRelative } from "@/lib/format";
import type { Project } from "@/api/types";

const COLUMNS: ReadonlyArray<Column<Project>> = [
  {
    id: "slug",
    header: "project",
    render: (row) => <span className="font-medium text-text">{row.slug}</span>,
  },
  { id: "user", header: "user", numeric: true, render: (row) => row.user },
  {
    id: "facts",
    header: "facts",
    numeric: true,
    align: "right",
    render: (row) => <Count value={row.counts.facts} />,
  },
  {
    id: "experiences",
    header: "experiences",
    numeric: true,
    align: "right",
    render: (row) => <Count value={row.counts.experiences} />,
  },
  {
    id: "sources",
    header: "sources",
    numeric: true,
    align: "right",
    render: (row) => <Count value={row.counts.sources} />,
  },
  {
    id: "activity",
    header: "last activity",
    numeric: true,
    render: (row) => (row.last_activity ? formatRelative(row.last_activity) : "never"),
  },
];

/** A zero recedes: it is the absence of something, not a measurement. */
function Count({ value }: { value: number }) {
  return <span className={value === 0 ? "text-text-5" : undefined}>{value}</span>;
}

export function ProjectTable({ projects }: { projects: readonly Project[] }) {
  return (
    <DataTable
      rows={projects}
      columns={COLUMNS}
      getRowKey={(row) => row.id}
      caption="Projects and what each one holds"
    />
  );
}
