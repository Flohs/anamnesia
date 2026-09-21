import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { UnitOverview } from "@/components/constellation/UnitOverview";
import type { DomainTotals } from "@/api/types";

const totals: DomainTotals = {
  users: 3,
  projects: 22,
  facts: 347,
  experiences: 253,
  skills: 0,
  working_memory: 0,
  entities: 140,
  edges: 81,
  sources: 539,
  commitments: 71,
  artifacts: 26,
};

describe("UnitOverview", () => {
  it("counts every unit the store holds", () => {
    render(<UnitOverview totals={totals} isolate={null} onIsolate={() => undefined} />);

    expect(screen.getByRole("button", { name: /^facts /i })).toHaveTextContent("347");
    expect(screen.getByRole("button", { name: /^entities /i })).toHaveTextContent("140");
  });

  /** A zero is information: it says the worker has not filled that in yet. */
  it("shows the empty domains rather than hiding them", () => {
    render(<UnitOverview totals={totals} isolate={null} onIsolate={() => undefined} />);

    expect(screen.getByRole("button", { name: /^skills /i })).toHaveTextContent("0");
  });

  it("isolates a domain the reader clicks", async () => {
    const onIsolate = vi.fn();
    const user = userEvent.setup();
    render(<UnitOverview totals={totals} isolate={null} onIsolate={onIsolate} />);

    await user.click(screen.getByRole("button", { name: /^entities /i }));

    expect(onIsolate).toHaveBeenCalledWith("entities");
  });

  it("lets the same click clear the isolation", async () => {
    const onIsolate = vi.fn();
    const user = userEvent.setup();
    render(<UnitOverview totals={totals} isolate="entities" onIsolate={onIsolate} />);

    await user.click(screen.getByRole("button", { name: /^entities /i }));

    expect(onIsolate).toHaveBeenCalledWith(null);
  });

  /** Only three domains carry vectors, so only those can light in the die. */
  it("does not offer to isolate a domain the die cannot show", async () => {
    const onIsolate = vi.fn();
    const user = userEvent.setup();
    render(<UnitOverview totals={totals} isolate={null} onIsolate={onIsolate} />);

    const sources = screen.getByRole("button", { name: /^sources /i });
    expect(sources).toBeDisabled();

    await user.click(sources);
    expect(onIsolate).not.toHaveBeenCalled();
  });

  it("marks the domain currently isolated", () => {
    render(<UnitOverview totals={totals} isolate="facts" onIsolate={() => undefined} />);

    expect(screen.getByRole("button", { name: /^facts /i })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: /^experiences /i })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
  });
});
