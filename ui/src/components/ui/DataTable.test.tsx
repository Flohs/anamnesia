import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { DataTable, type Column } from "@/components/ui/DataTable";

interface Row {
  id: string;
  name: string;
  count: number;
}

const columns: ReadonlyArray<Column<Row>> = [
  { id: "name", header: "name", render: (row) => row.name },
  { id: "count", header: "count", numeric: true, align: "right", render: (row) => row.count },
];

const rows: Row[] = [
  { id: "1", name: "anamnesia-ui", count: 12 },
  { id: "2", name: "zeroploy-frontend", count: 61 },
];

describe("DataTable", () => {
  it("renders a header per column and a row per record", () => {
    render(<DataTable rows={rows} columns={columns} getRowKey={(row) => row.id} caption="Projects" />);

    expect(screen.getAllByRole("columnheader")).toHaveLength(2);
    expect(screen.getAllByRole("row")).toHaveLength(3);
    expect(screen.getByText("zeroploy-frontend")).toBeInTheDocument();
  });

  it("shows the empty state instead of an empty table", () => {
    render(
      <DataTable
        rows={[]}
        columns={columns}
        getRowKey={(row) => row.id}
        empty={<p>No projects yet</p>}
      />,
    );

    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    expect(screen.getByText("No projects yet")).toBeInTheDocument();
  });

  it("still renders a table when empty and no empty state was supplied", () => {
    render(<DataTable rows={[]} columns={columns} getRowKey={(row) => row.id} />);
    expect(screen.getByRole("table")).toBeInTheDocument();
  });
});
