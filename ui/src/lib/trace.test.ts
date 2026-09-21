import { describe, expect, it } from "vitest";

import { buildStrip, segmentStyle, shortModel, stepTone } from "@/lib/trace";
import type { StepTiming } from "@/api/types";

const steps: StepTiming[] = [
  { name: "source", duration_ms: 2 },
  { name: "gate", duration_ms: 310 },
  { name: "similar", duration_ms: 118 },
  { name: "llm", duration_ms: 1290, detail: { model: "openai/gpt-4o-mini" } },
  { name: "ops", duration_ms: 96 },
  { name: "apply", duration_ms: 24 },
];

describe("buildStrip", () => {
  it("sizes each segment by its share of the total", () => {
    const strip = buildStrip(steps, "ok");

    expect(strip.totalMs).toBe(1840);
    expect(strip.segments).toHaveLength(6);
    expect(strip.segments[3]?.share).toBeCloseTo(1290 / 1840, 5);
  });

  it("labels only the segments wide enough to hold text", () => {
    const labelled = buildStrip(steps, "ok")
      .segments.filter((segment) => segment.showLabel)
      .map((segment) => segment.name);

    // gate at 17%, similar at 6.4% and llm at 70% have room; ops at 5.2%
    // and a 2 ms source do not, so their labels live in the tooltip.
    expect(labelled).toEqual(["gate", "similar", "llm"]);
    expect(labelled).not.toContain("source");
  });

  it("names the model in the label when the step reports one", () => {
    const llm = buildStrip(steps, "ok").segments.find((segment) => segment.name === "llm");
    expect(llm?.label).toBe("llm · gpt-4o-mini");
  });

  it("marks a failed trace as truncated so the bar can show where it stopped", () => {
    expect(buildStrip(steps, "failed").truncated).toBe(true);
    expect(buildStrip(steps, "ok").truncated).toBe(false);
  });

  it("keeps zero-duration steps visible instead of dropping them", () => {
    const strip = buildStrip([{ name: "source", duration_ms: 0 }], "ok");
    expect(strip.segments).toHaveLength(1);
  });

  it("divides width evenly when nothing took measurable time", () => {
    const strip = buildStrip(
      [
        { name: "source", duration_ms: 0 },
        { name: "gate", duration_ms: 0 },
      ],
      "ok",
    );

    expect(strip.segments.every((segment) => segment.share === 0.5)).toBe(true);
  });

  it("renders an empty step list as an empty strip rather than throwing", () => {
    expect(buildStrip([], "ok").segments).toEqual([]);
  });
});

describe("segmentStyle", () => {
  it("never gives a segment zero flex, which would collapse it", () => {
    const [segment] = buildStrip([{ name: "source", duration_ms: 0 }], "ok").segments;
    expect(segmentStyle(segment!).flexGrow).toBeGreaterThan(0);
  });
});

describe("stepTone", () => {
  it("gives model calls the accent tone", () => {
    expect(stepTone("llm")).toBe("run");
  });

  it("falls back to neutral for a step name it has never seen", () => {
    expect(stepTone("some-future-step")).toBe("neutral");
  });
});

describe("shortModel", () => {
  it("drops the vendor prefix", () => {
    expect(shortModel("openai/gpt-4o-mini")).toBe("gpt-4o-mini");
    expect(shortModel("gpt-4o-mini")).toBe("gpt-4o-mini");
  });
});
