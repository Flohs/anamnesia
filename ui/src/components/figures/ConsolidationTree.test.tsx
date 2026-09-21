import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { ConsolidationTree } from "@/components/figures/ConsolidationTree";
import type { Experience } from "@/api/types";

function experience(overrides: Partial<Experience> & Pick<Experience, "id">): Experience {
  return {
    scope: { user_id: "user-1", project_id: "project-1" },
    kind: "case",
    abstraction: 0,
    body: "body",
    trust: 0.7,
    importance: 0.5,
    relevance: 0.5,
    use_count: 0,
    occurred_at: "2026-08-21T20:00:00Z",
    ingested_at: "2026-08-21T20:00:00Z",
    ...overrides,
  };
}

/** Edges are the only <path> the figure draws; nodes are circles, levels text. */
function edgeCount(container: HTMLElement): number {
  return container.querySelectorAll("path").length;
}

describe("ConsolidationTree", () => {
  /**
   * The server writes the summary at abstraction 1 carrying parent_id pointing
   * *down* at one representative member, and the full lineage in
   * meta.consolidated_from. Both members should be linked to the summary.
   */
  it("draws an edge from every folded source up to the summary", () => {
    const { container } = render(
      <ConsolidationTree
        experiences={[
          experience({ id: "src-a", title: "raw a" }),
          experience({ id: "src-b", title: "raw b" }),
          experience({
            id: "summary",
            title: "the fold",
            abstraction: 1,
            parent_id: "src-a",
            meta: { consolidated_from: ["src-a", "src-b"] },
          }),
        ]}
      />,
    );

    expect(edgeCount(container)).toBe(2);
  });

  /** Rows written before consolidated_from existed still carry parent_id. */
  it("falls back to parent_id when the summary has no consolidated_from", () => {
    const { container } = render(
      <ConsolidationTree
        experiences={[
          experience({ id: "src-a", title: "raw a" }),
          experience({ id: "summary", title: "the fold", abstraction: 1, parent_id: "src-a" }),
        ]}
      />,
    );

    expect(edgeCount(container)).toBe(1);
  });

  /** A parent at the same level is a supersede link, not a fold. */
  it("ignores a link between two rows at the same abstraction", () => {
    render(
      <ConsolidationTree
        experiences={[
          experience({ id: "older", title: "older" }),
          experience({ id: "newer", title: "newer", parent_id: "older" }),
        ]}
      />,
    );

    expect(screen.getByText(/nothing to draw as a fold/i)).toBeInTheDocument();
  });

  it("reads out a summary's level and how many rows it folded", async () => {
    const user = userEvent.setup();
    const { container } = render(
      <ConsolidationTree
        experiences={[
          experience({ id: "src-a", title: "raw a" }),
          experience({ id: "src-b", title: "raw b" }),
          experience({
            id: "summary",
            title: "the fold",
            kind: "strategy",
            abstraction: 1,
            meta: { consolidated_from: ["src-a", "src-b"] },
          }),
        ]}
      />,
    );

    // Nodes are drawn raw first, so the summary is the last circle.
    const circles = container.querySelectorAll("circle");
    await user.hover(circles[circles.length - 1] as Element);

    const tooltip = screen.getByRole("tooltip");
    expect(tooltip).toHaveTextContent("the fold");
    expect(tooltip).toHaveTextContent(/level\s*fold 1/);
    expect(tooltip).toHaveTextContent(/folded\s*2 rows/);
    expect(tooltip).toHaveTextContent(/kind\s*strategy/);
  });

  it("stops carrying a native title, which would double up on the readout", () => {
    const { container } = render(
      <ConsolidationTree
        experiences={[
          experience({ id: "src-a", title: "raw a" }),
          experience({ id: "summary", title: "the fold", abstraction: 1, parent_id: "src-a" }),
        ]}
      />,
    );

    expect(container.querySelectorAll("title")).toHaveLength(0);
  });

  /** A source outside the fetched page must not take its summary down with it. */
  it("draws the members it has when one folded source is missing", () => {
    const { container } = render(
      <ConsolidationTree
        experiences={[
          experience({ id: "src-a", title: "raw a" }),
          experience({
            id: "summary",
            title: "the fold",
            abstraction: 1,
            parent_id: "src-a",
            meta: { consolidated_from: ["src-a", "src-off-page"] },
          }),
        ]}
      />,
    );

    expect(edgeCount(container)).toBe(1);
  });
});
