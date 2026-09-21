import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { ArtifactRing } from "@/components/constellation/ArtifactRing";
import type { Artifact } from "@/api/types";

const artifacts = [
  {
    id: "a",
    scope: { user_id: "u" },
    title: "Memory Constellation",
    project: "anamnesia",
    url: "https://claude.ai/code/artifact/abc",
  },
  {
    id: "b",
    scope: { user_id: "u" },
    title: "Memory Star Map",
    project: "anamnesia",
    url: "https://claude.ai/code/artifact/def",
  },
  { id: "c", scope: { user_id: "u" }, title: "Custom hostnames", project: "zeroploy" },
] as Artifact[];

function ring(props: Partial<Parameters<typeof ArtifactRing>[0]> = {}) {
  return render(
    <ArtifactRing
      artifacts={artifacts}
      cloud={[]}
      lit={null}
      onHover={() => undefined}
      hueOf={() => "#ff7a45"}
      {...props}
    />,
  );
}

describe("ArtifactRing", () => {
  it("puts one tile in the ring per artifact, named for a screen reader", () => {
    ring();

    expect(screen.getByRole("link", { name: /Memory Constellation/ })).toBeInTheDocument();
    expect(screen.getByLabelText(/Custom hostnames/)).toBeInTheDocument();
  });

  it("reports the tile it is pointing at, so the list can light the same row", async () => {
    const onHover = vi.fn();
    const user = userEvent.setup();
    ring({ onHover });

    await user.hover(screen.getByRole("link", { name: /Memory Star Map/ }));

    expect(onHover).toHaveBeenCalledWith(1);
  });

  /** The behaviour the whole control exists for. */
  it("flies every name in while the i is hovered, and takes them away after", async () => {
    const user = userEvent.setup();
    const { container } = ring();
    const names = () => container.querySelectorAll("[data-name][data-shown='true']");

    expect(names()).toHaveLength(0);

    await user.hover(screen.getByRole("button", { name: /show every artifact name/i }));
    expect(names()).toHaveLength(3);

    await user.unhover(screen.getByRole("button", { name: /show every artifact name/i }));
    expect(names()).toHaveLength(0);
  });

  it("opens a search field when the i is clicked", async () => {
    const user = userEvent.setup();
    ring();

    expect(screen.queryByRole("searchbox")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /show every artifact name/i }));

    expect(screen.getByRole("searchbox")).toBeInTheDocument();
  });

  it("counts the matches and lights only those tiles", async () => {
    const user = userEvent.setup();
    const { container } = ring();

    await user.click(screen.getByRole("button", { name: /show every artifact name/i }));
    await user.type(screen.getByRole("searchbox"), "memory");

    expect(screen.getByText(/2 matches/i)).toBeInTheDocument();
    expect(container.querySelectorAll("[data-hit='true']")).toHaveLength(2);
  });

  it("matches on the project too, not only the name", async () => {
    const user = userEvent.setup();
    ring();

    await user.click(screen.getByRole("button", { name: /show every artifact name/i }));
    await user.type(screen.getByRole("searchbox"), "zeroploy");

    expect(screen.getByText(/1 match\b/i)).toBeInTheDocument();
  });

  it("closes the search on Escape and stops dimming", async () => {
    const user = userEvent.setup();
    const { container } = ring();

    await user.click(screen.getByRole("button", { name: /show every artifact name/i }));
    await user.type(screen.getByRole("searchbox"), "memory");
    await user.keyboard("{Escape}");

    expect(screen.queryByRole("searchbox")).not.toBeInTheDocument();
    expect(container.querySelectorAll("[data-hit='true']")).toHaveLength(0);
  });

  it("opens the artifact itself when a tile is clicked", () => {
    ring();

    const tile = screen.getByRole("link", { name: /Memory Constellation/ });
    expect(tile).toHaveAttribute("href", "https://claude.ai/code/artifact/abc");
    expect(tile).toHaveAttribute("target", "_blank");
    expect(tile.getAttribute("rel")).toContain("noreferrer");
  });

  it("does not pretend a tile is a link when no URL was captured", () => {
    ring();

    expect(screen.queryByRole("link", { name: /Custom hostnames/ })).not.toBeInTheDocument();
    expect(screen.getByLabelText(/Custom hostnames/)).toBeInTheDocument();
  });

  it("says so when nothing has been captured yet", () => {
    ring({ artifacts: [] });

    expect(screen.getByText(/no artifacts yet/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /show every artifact name/i })).not.toBeInTheDocument();
  });

  it("keeps the tile the list is pointing at lit", () => {
    const { container } = ring({ lit: 2 });

    const tile = screen.getByLabelText(/Custom hostnames/);
    expect(tile).toHaveAttribute("aria-current", "true");
    expect(within(container).getByText("Custom hostnames")).toBeInTheDocument();
  });
});
