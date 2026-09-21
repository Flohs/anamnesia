import { describe, expect, it } from "vitest";

import { halfLifeDays, parseDurationHours } from "@/lib/config";
import type { Config } from "@/api/types";

// Keys and values copied from a live `/v1/config`, so a rename upstream
// breaks this test rather than silently reverting the chart to a guess.
const config: Config = {
  items: [
    { key: "decay.half_life_case", value: "336h", source: "default", secret: false },
    { key: "decay.half_life_strategy", value: "8760h", source: "default", secret: false },
    { key: "decay.half_life_hybrid", value: "1440h", source: "default", secret: false },
  ],
};

describe("parseDurationHours", () => {
  it.each([
    ["504h", 504],
    ["30m", 0.5],
    ["1h30m", 1.5],
  ])("parses %s", (value, expected) => {
    expect(parseDurationHours(value)).toBeCloseTo(expected, 5);
  });

  it("returns undefined for something that is not a duration", () => {
    expect(parseDurationHours("soon")).toBeUndefined();
  });
});

describe("halfLifeDays", () => {
  it("reads the configured half-life rather than assuming one", () => {
    expect(halfLifeDays(config, "case", 21)).toBe(14);
    expect(halfLifeDays(config, "strategy", 365)).toBe(365);
    expect(halfLifeDays(config, "hybrid", 30)).toBe(60);
  });

  it("falls back when the server has not reported that setting", () => {
    expect(halfLifeDays(undefined, "case", 21)).toBe(21);
    expect(halfLifeDays(config, "nonexistent-kind", 40)).toBe(40);
  });
});
