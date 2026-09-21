import { render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ErrorBoundary } from "@/components/ui/ErrorBoundary";

function Boom({ throws }: { throws: boolean }): React.ReactNode {
  if (throws) throw new TypeError("Cannot read properties of undefined (reading 'slice')");
  return <p>the panel rendered</p>;
}

describe("ErrorBoundary", () => {
  beforeEach(() => {
    // React logs every caught error to console.error. The boundary working is
    // the assertion; the noise would drown the rest of the run.
    vi.spyOn(console, "error").mockImplementation(() => undefined);
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("shows its children when nothing throws", () => {
    render(
      <ErrorBoundary label="Shape">
        <Boom throws={false} />
      </ErrorBoundary>,
    );

    expect(screen.getByText("the panel rendered")).toBeInTheDocument();
  });

  it("replaces a throwing child with a diagnostic instead of a blank page", () => {
    render(
      <ErrorBoundary label="Shape">
        <Boom throws />
      </ErrorBoundary>,
    );

    expect(screen.getByRole("status")).toHaveTextContent(/Shape/);
    expect(screen.getByRole("status")).toHaveTextContent(/reading 'slice'/);
  });

  /** One bad panel must not take the panels beside it down with it. */
  it("leaves a sibling boundary untouched", () => {
    render(
      <>
        <ErrorBoundary label="FIG. 01">
          <Boom throws />
        </ErrorBoundary>
        <ErrorBoundary label="FIG. 02">
          <Boom throws={false} />
        </ErrorBoundary>
      </>,
    );

    expect(screen.getByText("the panel rendered")).toBeInTheDocument();
    expect(screen.getAllByRole("status")).toHaveLength(1);
  });

  /**
   * Without this the reader is stuck: navigating away from a broken view would
   * keep showing the failure, because a boundary holds its error state.
   */
  it("recovers when the reset key changes", () => {
    const { rerender } = render(
      <ErrorBoundary label="Memory" resetKey="sources">
        <Boom throws />
      </ErrorBoundary>,
    );

    expect(screen.getByRole("status")).toBeInTheDocument();

    rerender(
      <ErrorBoundary label="Memory" resetKey="facts">
        <Boom throws={false} />
      </ErrorBoundary>,
    );

    expect(screen.getByText("the panel rendered")).toBeInTheDocument();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });
});
