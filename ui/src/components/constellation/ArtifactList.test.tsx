import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { ArtifactList } from "@/components/constellation/ArtifactList";
import type { Artifact } from "@/api/types";

const artifacts = [
  {
    id: "a",
    scope: { user_id: "u" },
    title: "Memory Constellation",
    description: "A ring of artifacts.",
    url: "https://claude.ai/code/artifact/abc",
    project: "anamnesia",
    created_at: "2026-08-24T10:00:00Z",
  },
  {
    id: "b",
    scope: { user_id: "u" },
    description: "Interactive mockup of the blueprint editor.",
    project: "zeroploy",
    created_at: "2026-08-23T10:00:00Z",
  },
] as Artifact[];

describe("ArtifactList", () => {
  it("names every artifact, falling back to the description when untitled", () => {
    render(<ArtifactList artifacts={artifacts} lit={null} onHover={() => undefined} hueOf={() => "#ff7a45"} />);

    expect(screen.getByText("Memory Constellation")).toBeInTheDocument();
    expect(screen.getByText("Interactive mockup of the blueprint editor")).toBeInTheDocument();
  });

  it("shows which project each one came from", () => {
    render(<ArtifactList artifacts={artifacts} lit={null} onHover={() => undefined} hueOf={() => "#ff7a45"} />);

    expect(screen.getByText("anamnesia")).toBeInTheDocument();
    expect(screen.getByText("zeroploy")).toBeInTheDocument();
  });

  /** The ring and the list are two views of one selection. */
  it("reports the index it is pointing at, so the ring can light the same tile", async () => {
    const onHover = vi.fn();
    const user = userEvent.setup();
    render(<ArtifactList artifacts={artifacts} lit={null} onHover={onHover} hueOf={() => "#ff7a45"} />);

    await user.hover(screen.getByText("Interactive mockup of the blueprint editor").closest("[data-row]")!);

    expect(onHover).toHaveBeenCalledWith(1);
  });

  it("marks the row the ring is pointing at", () => {
    render(<ArtifactList artifacts={artifacts} lit={1} onHover={() => undefined} hueOf={() => "#ff7a45"} />);

    expect(screen.getByText("Interactive mockup of the blueprint editor").closest("[data-row]")!).toHaveAttribute(
      "aria-current",
      "true",
    );
    expect(screen.getByRole("link", { name: /Memory Constellation/ })).not.toHaveAttribute(
      "aria-current",
    );
  });

  it("dims everything but the matches while a search is running", () => {
    render(
      <ArtifactList artifacts={artifacts} lit={null} onHover={() => undefined} matches={[1]} hueOf={() => "#ff7a45"} />,
    );

    expect(screen.getByText("Interactive mockup of the blueprint editor").closest("[data-row]")!).toHaveAttribute(
      "data-hit",
      "true",
    );
    expect(screen.getByRole("link", { name: /Memory Constellation/ })).toHaveAttribute(
      "data-hit",
      "false",
    );
  });

  it("opens the artifact itself when a row is clicked", () => {
    render(<ArtifactList artifacts={artifacts} lit={null} onHover={() => undefined} hueOf={() => "#ff7a45"} />);

    const row = screen.getByRole("link", { name: /Memory Constellation/ });
    expect(row).toHaveAttribute("href", "https://claude.ai/code/artifact/abc");
    expect(row).toHaveAttribute("target", "_blank");
    // The artifact is on another origin, so the opener must not travel with it.
    expect(row.getAttribute("rel")).toContain("noreferrer");
  });

  /** Every artifact on the live install has a URL, but the schema allows none. */
  it("does not pretend a row is a link when no URL was captured", () => {
    render(<ArtifactList artifacts={artifacts} lit={null} onHover={() => undefined} hueOf={() => "#ff7a45"} />);

    expect(screen.queryByRole("link", { name: /Interactive mockup/ })).not.toBeInTheDocument();
    expect(screen.getByText("Interactive mockup of the blueprint editor")).toBeInTheDocument();
  });

  it("says so when nothing has been captured yet", () => {
    render(<ArtifactList artifacts={[]} lit={null} onHover={() => undefined} hueOf={() => "#ff7a45"} />);

    expect(screen.getByText(/no artifacts yet/i)).toBeInTheDocument();
  });
});
