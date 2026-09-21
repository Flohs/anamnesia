import { describe, expect, it } from "vitest";

import { artifactName } from "@/lib/artifact";

describe("artifactName", () => {
  it("uses the title when the capture recorded one", () => {
    expect(artifactName({ title: "Memory Constellation", description: "ignored" })).toBe(
      "Memory Constellation",
    );
  });

  it("treats a whitespace-only title as absent", () => {
    expect(artifactName({ title: "  ", description: "A zone creation walkthrough." })).toBe(
      "A zone creation walkthrough",
    );
  });

  /**
   * Twelve of the twenty-six artifacts on the live install arrive with no
   * title at all, and every one of them has a description. A ring of
   * "untitled" tiles would be worse than useless.
   */
  it("falls back to the description when no title was captured", () => {
    expect(artifactName({ description: "Interactive mockup of the blueprint editor." })).toBe(
      "Interactive mockup of the blueprint editor",
    );
  });

  it("drops the trailing full stop, because a label is not a sentence", () => {
    expect(artifactName({ description: "Three placements for the banner." })).toBe(
      "Three placements for the banner",
    );
  });

  it("truncates a long description to something a tile can carry", () => {
    const description =
      "A detailed interactive walkthrough of every DNS and SSL setup path available to a customer, including the certificate renewal flow.";
    const name = artifactName({ description });

    expect(name.length).toBeLessThanOrEqual(60);
    expect(name.startsWith("A detailed interactive walkthrough")).toBe(true);
  });

  it("says so plainly when neither a title nor a description exists", () => {
    expect(artifactName({})).toBe("Untitled artifact");
    expect(artifactName({ title: "", description: "   " })).toBe("Untitled artifact");
  });
});
