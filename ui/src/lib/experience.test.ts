import { describe, expect, it } from "vitest";

import { experienceTitle } from "@/lib/experience";

describe("experienceTitle", () => {
  it("uses the title when the extractor supplied one", () => {
    expect(experienceTitle({ title: "Hook ordering is guaranteed", body: "ignored" })).toBe(
      "Hook ordering is guaranteed",
    );
  });

  it("treats a whitespace-only title as absent", () => {
    expect(experienceTitle({ title: "   ", body: "The gate rejected it." })).toBe(
      "The gate rejected it.",
    );
  });

  it("truncates a long single-sentence body to fit a table column", () => {
    // A real untitled row from the live install: one sentence, 166 characters.
    const body =
      "Implemented a systematic approach to debugging within the emulator, ensuring all changes maintain fidelity to the controller API and improving overall functionality.";
    const title = experienceTitle({ body });

    expect(title.length).toBeLessThanOrEqual(90);
    expect(title.startsWith("Implemented a systematic approach")).toBe(true);
  });

  it("stops at the first sentence when a body has several", () => {
    expect(
      experienceTitle({ body: "Reranking changed the order. The top result stayed put." }),
    ).toBe("Reranking changed the order.");
  });

  it("truncates a long unpunctuated body rather than returning all of it", () => {
    const title = experienceTitle({ body: "a".repeat(400) });
    expect(title.length).toBeLessThanOrEqual(90);
    expect(title.endsWith("…")).toBe(true);
  });

  it("never returns an empty string", () => {
    expect(experienceTitle({ body: "" })).toBe("Untitled");
  });
});
