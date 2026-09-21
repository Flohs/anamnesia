import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { DecayCurve } from "@/components/figures/DecayCurve";
import type { Experience } from "@/api/types";

/** Fixed age so the readout's "age" row is assertable without mocking time. */
const TEN_DAYS_AGO = new Date(Date.now() - 10 * 86_400_000).toISOString();

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
    occurred_at: TEN_DAYS_AGO,
    ingested_at: TEN_DAYS_AGO,
    ...overrides,
  };
}

describe("DecayCurve", () => {
  it("reads out the hovered dot's age, relevance and kind", async () => {
    const user = userEvent.setup();
    const { container } = render(
      <DecayCurve
        experiences={[experience({ id: "a", title: "a decaying case", relevance: 0.42 })]}
      />,
    );

    const dot = container.querySelector("circle");
    expect(dot).not.toBeNull();
    await user.hover(dot as Element);

    const tooltip = screen.getByRole("tooltip");
    expect(tooltip).toHaveTextContent("a decaying case");
    expect(tooltip).toHaveTextContent(/kind\s*case/);
    expect(tooltip).toHaveTextContent(/age\s*10d/);
    expect(tooltip).toHaveTextContent(/relevance\s*0\.42/);
  });

  it("stops carrying a native title, which would double up on the readout", () => {
    const { container } = render(
      <DecayCurve experiences={[experience({ id: "a", title: "a decaying case" })]} />,
    );

    expect(container.querySelectorAll("title")).toHaveLength(0);
  });
});
