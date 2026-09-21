import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { ActivityHeatmap } from "@/components/figures/ActivityHeatmap";
import type { ActivityBucket } from "@/api/types";

/**
 * The server groups by project, and a source ingested outside any project comes
 * back with project null. The type says string, so nothing above this component
 * strips it.
 */
const bucketsWithUnscopedRow = [
  { date: "2026-08-18", project: null, sources: 1, facts: 0, experiences: 0 },
  { date: "2026-08-18", project: "anamnesia-ui", sources: 2, facts: 2, experiences: 2 },
] as unknown as ActivityBucket[];

describe("ActivityHeatmap", () => {
  it("draws a row for a bucket that belongs to no project", () => {
    render(<ActivityHeatmap buckets={bucketsWithUnscopedRow} />);

    expect(screen.getByText("anamnesia-ui")).toBeInTheDocument();
    expect(screen.getByText("(no project)")).toBeInTheDocument();
  });

  it("keeps the counts of an unscoped bucket separate from a named project", () => {
    render(<ActivityHeatmap buckets={bucketsWithUnscopedRow} />);

    expect(screen.getAllByRole("img")).toHaveLength(1);
    expect(screen.getByText("(no project)")).toBeInTheDocument();
  });

  /**
   * The colour carries one metric, but the cell knows all three, and the
   * question a reader has at a dark cell is "quiet in what sense".
   */
  it("reads out every count for the hovered cell", async () => {
    const today = new Date().toISOString().slice(0, 10);
    const user = userEvent.setup();

    const { container } = render(
      <ActivityHeatmap
        days={1}
        buckets={[{ date: today, project: "anamnesia-ui", sources: 4, facts: 2, experiences: 1 }]}
      />,
    );

    const cell = container.querySelector("rect");
    expect(cell).not.toBeNull();
    await user.hover(cell as Element);

    const tooltip = screen.getByRole("tooltip");
    expect(tooltip).toHaveTextContent("anamnesia-ui");
    expect(tooltip).toHaveTextContent(today);
    expect(tooltip).toHaveTextContent(/sources\s*4/);
    expect(tooltip).toHaveTextContent(/facts\s*2/);
    expect(tooltip).toHaveTextContent(/experiences\s*1/);
  });

  it("stops carrying a native title, which would double up on the readout", () => {
    const { container } = render(<ActivityHeatmap buckets={bucketsWithUnscopedRow} />);

    expect(container.querySelectorAll("title")).toHaveLength(0);
  });
});
