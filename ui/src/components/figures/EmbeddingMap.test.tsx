import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { EmbeddingMap } from "@/components/figures/EmbeddingMap";
import type { EmbeddingMap as EmbeddingMapData, EmbeddingPoint } from "@/api/types";

function point(index: number, overrides: Partial<EmbeddingPoint> = {}): EmbeddingPoint {
  return {
    id: `point-${index}`,
    title: `memory ${index}`,
    project: "anamnesia-ui",
    kind: "case",
    x: index,
    y: index % 3,
    ...overrides,
  };
}

/** The figure refuses to plot below MIN_MEANINGFUL_POINTS, so give it enough. */
function data(overrides: Partial<EmbeddingMapData> = {}): EmbeddingMapData {
  return {
    domain: "experiences",
    n: 10,
    dims: 1536,
    explained_variance: [0.06, 0.05],
    points: Array.from({ length: 10 }, (_, index) => point(index)),
    ...overrides,
  };
}

describe("EmbeddingMap", () => {
  it("reads out the hovered point's project and cluster", async () => {
    const user = userEvent.setup();
    const points = Array.from({ length: 10 }, (_, index) => point(index));
    points[0] = point(0, { title: "the first memory", project: "smoxy", kind: "strategy" });

    const { container } = render(<EmbeddingMap data={data({ points })} />);

    const dot = container.querySelector("circle");
    expect(dot).not.toBeNull();
    await user.hover(dot as Element);

    const tooltip = screen.getByRole("tooltip");
    expect(tooltip).toHaveTextContent("the first memory");
    expect(tooltip).toHaveTextContent(/project\s*smoxy/);
    expect(tooltip).toHaveTextContent(/cluster\s*strategy/);
  });

  it("stops carrying a native title, which would double up on the readout", () => {
    const { container } = render(<EmbeddingMap data={data()} />);

    expect(container.querySelectorAll("title")).toHaveLength(0);
  });
});
