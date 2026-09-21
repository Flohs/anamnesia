import { describe, expect, it } from "vitest";

import { domainHue, projectHues } from "@/components/constellation/hues";

describe("projectHues", () => {
  /**
   * A hash of the slug cannot promise two projects different colours, and on
   * the live set of four it collided. Assigning from the projects actually
   * present can promise it, up to the size of the wheel.
   */
  it("gives every project on a real install its own colour", () => {
    const hueOf = projectHues(["anamnesia", "zeroploy", "smoxy", "ash"]);
    const hues = ["anamnesia", "zeroploy", "smoxy", "ash"].map(hueOf);

    expect(new Set(hues).size).toBe(4);
  });

  it("does not care what order the server listed them in", () => {
    const one = projectHues(["ash", "zeroploy", "anamnesia", "smoxy"]);
    const other = projectHues(["anamnesia", "smoxy", "ash", "zeroploy"]);

    expect(one("zeroploy")).toBe(other("zeroploy"));
  });

  it("ignores repeats, so a project with many artifacts does not eat two hues", () => {
    const hueOf = projectHues(["anamnesia", "anamnesia", "anamnesia", "zeroploy"]);

    expect(hueOf("anamnesia")).not.toBe(hueOf("zeroploy"));
  });

  it("answers for an unscoped artifact and for one it has never seen", () => {
    const hueOf = projectHues(["anamnesia"]);

    expect(hueOf(null)).toMatch(/^#[0-9a-f]{6}$/i);
    expect(hueOf("never-seen")).toMatch(/^#[0-9a-f]{6}$/i);
  });
});

describe("domainHue", () => {
  /** These are the console's own semantic hues, not new ones. */
  it("keeps a fact the same blue it is in the tables", () => {
    expect(domainHue("facts")).toBe("#7cc7f0");
    expect(domainHue("experiences")).toBe("#ff7a45");
  });

  it("answers for a domain the console does not colour yet", () => {
    expect(domainHue("working_memory")).toMatch(/^#[0-9a-f]{6}$/i);
  });
});
