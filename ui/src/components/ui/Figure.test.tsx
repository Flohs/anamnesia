import { render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Figure } from "@/components/ui/Figure";

function Boom(): React.ReactNode {
  throw new TypeError("Cannot read properties of null (reading 'length')");
}

describe("Figure", () => {
  it("frames its content", () => {
    render(
      <Figure number="FIG. 01" title="Consolidation folds detail upward">
        <p>a drawing</p>
      </Figure>,
    );

    expect(screen.getByText("a drawing")).toBeInTheDocument();
    expect(screen.getByText("Consolidation folds detail upward")).toBeInTheDocument();
  });

  describe("when a figure throws while rendering", () => {
    beforeEach(() => {
      vi.spyOn(console, "error").mockImplementation(() => undefined);
    });

    afterEach(() => {
      vi.restoreAllMocks();
    });

    /**
     * The Shape view puts four figures on one page. A field the server declined
     * to send should cost the reader that one card, not the console.
     */
    it("keeps the failure inside its own card", () => {
      render(
        <>
          <Figure number="FIG. 01" title="Consolidation folds detail upward">
            <Boom />
          </Figure>
          <Figure number="FIG. 02" title="Where memory accumulated">
            <p>the heatmap</p>
          </Figure>
        </>,
      );

      expect(screen.getByText("the heatmap")).toBeInTheDocument();
      expect(screen.getByRole("status")).toHaveTextContent(/FIG\. 01/);
      expect(screen.getByRole("status")).toHaveTextContent(/reading 'length'/);
    });

    it("still shows the failed figure's own heading, so the page keeps its shape", () => {
      render(
        <Figure number="FIG. 03" title="What it is forgetting, and how fast">
          <Boom />
        </Figure>,
      );

      expect(screen.getByText("What it is forgetting, and how fast")).toBeInTheDocument();
    });
  });
});
