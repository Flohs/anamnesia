import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { TraceFeed } from "@/components/activity/TraceFeed";
import { renderWithProviders } from "@/test/render";
import type { TraceSummary } from "@/api/types";

function trace(overrides: Partial<TraceSummary> = {}): TraceSummary {
  return {
    id: "a1f4c8d2",
    seq: 41,
    kind: "ingest",
    status: "ok",
    user: "default",
    project: "anamnesia-ui",
    started_at: new Date(Date.now() - 12_000).toISOString(),
    ended_at: new Date().toISOString(),
    duration_ms: 1840,
    summary: "Kept a 4.2 kB checkpoint and wrote 1 fact, 1 experience",
    step_count: 6,
    step_timings: [
      { name: "gate", duration_ms: 310 },
      { name: "llm", duration_ms: 1290 },
      { name: "apply", duration_ms: 240 },
    ],
    ...overrides,
  };
}

describe("TraceFeed", () => {
  it("invites the user to act when there is nothing to show", () => {
    renderWithProviders(<TraceFeed traces={[]} onOpen={vi.fn()} />);

    expect(screen.getByText("Nothing has happened yet")).toBeInTheDocument();
    expect(screen.getByText(/live in memory/i)).toBeInTheDocument();
  });

  it("shows the summary, the project and a strip of the real step durations", () => {
    renderWithProviders(<TraceFeed traces={[trace()]} onOpen={vi.fn()} />);

    const row = screen.getByRole("button");
    expect(within(row).getByText(/Kept a 4.2 kB checkpoint/)).toBeInTheDocument();
    expect(within(row).getByText("anamnesia-ui")).toBeInTheDocument();
    expect(within(row).getByTitle("llm · 1 290 ms")).toBeInTheDocument();
    expect(within(row).getByText("1 840 ms")).toBeInTheDocument();
  });

  it("opens the trace it was given, not an index into the list", async () => {
    const onOpen = vi.fn();
    renderWithProviders(<TraceFeed traces={[trace({ id: "first" }), trace({ id: "second" })]} onOpen={onOpen} />);

    await userEvent.click(screen.getAllByRole("button")[1]!);

    expect(onOpen).toHaveBeenCalledWith("second");
  });

  it("labels a failure but leaves a skip unlabelled as a fault", () => {
    const { rerender } = renderWithProviders(<TraceFeed traces={[trace({ status: "failed" })]} onOpen={vi.fn()} />);
    expect(screen.getByText("failed")).toBeInTheDocument();
    expect(screen.getByText("stopped here")).toBeInTheDocument();

    rerender(<TraceFeed traces={[trace({ status: "skipped" })]} onOpen={vi.fn()} />);
    expect(screen.getByText("skipped")).toBeInTheDocument();
    expect(screen.queryByText("stopped here")).not.toBeInTheDocument();
  });
});

describe("TraceFeed summaries", () => {
  it("flattens a multi-line prompt onto one line", () => {
    renderWithProviders(
      <TraceFeed
        traces={[trace({ summary: String.raw`Returned 0 memories for "Item 1\n\n- a deviation"` })]}
        onOpen={vi.fn()}
      />,
    );

    expect(screen.getByText(/Returned 0 memories for "Item 1 - a deviation"/)).toBeInTheDocument();
  });

  it("omits the project chip for a trace that spans every scope", () => {
    renderWithProviders(
      <TraceFeed traces={[trace({ kind: "consolidate", project: "" })]} onOpen={vi.fn()} />,
    );

    // The kind label remains; only the empty project pill is gone.
    expect(screen.getByText("consolidate")).toBeInTheDocument();
    expect(screen.queryByText("anamnesia-ui")).not.toBeInTheDocument();
  });
});
