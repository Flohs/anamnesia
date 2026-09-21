import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { FigureTooltip } from "@/components/ui/FigureTooltip";
import { useFigureTooltip } from "@/components/ui/useFigureTooltip";

/** A figure in miniature: one hoverable shape, wired the way the real ones are. */
function Harness() {
  const tooltip = useFigureTooltip();

  return (
    <div ref={tooltip.containerRef}>
      <svg role="img" aria-label="a figure">
        <rect
          data-testid="cell"
          width={10}
          height={10}
          {...tooltip.hoverProps({
            title: "anamnesia-ui",
            rows: [
              { label: "date", value: "2026-08-18" },
              { label: "sources", value: "4" },
            ],
          })}
        />
      </svg>
      <FigureTooltip state={tooltip.state} />
    </div>
  );
}

describe("FigureTooltip", () => {
  it("stays out of the way until a shape is hovered", () => {
    render(<Harness />);

    expect(screen.queryByRole("tooltip")).not.toBeInTheDocument();
  });

  it("reads out the hovered shape's title and values", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.hover(screen.getByTestId("cell"));

    const tooltip = screen.getByRole("tooltip");
    expect(tooltip).toHaveTextContent("anamnesia-ui");
    expect(tooltip).toHaveTextContent("date");
    expect(tooltip).toHaveTextContent("2026-08-18");
    expect(tooltip).toHaveTextContent("sources");
    expect(tooltip).toHaveTextContent("4");
  });

  it("clears when the pointer leaves the shape", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.hover(screen.getByTestId("cell"));
    expect(screen.getByRole("tooltip")).toBeInTheDocument();

    await user.unhover(screen.getByTestId("cell"));
    expect(screen.queryByRole("tooltip")).not.toBeInTheDocument();
  });
});
