import { describe, expect, it } from "vitest";

import { factValue } from "@/lib/fact";

describe("factValue", () => {
  it("unwraps the {v: …} scalar wrapper the extractor writes", () => {
    expect(factValue({ v: 8 })).toBe("8");
    expect(factValue({ v: "v0.1.0-rc3" })).toBe("v0.1.0-rc3");
  });

  it("does not quote a string it unwrapped", () => {
    expect(factValue({ v: "openrouter" })).not.toContain('"');
  });

  it("shows a structured value as compact JSON", () => {
    expect(factValue({ items: ["/logs/stream", "/metrics/stream"] })).toBe(
      '{"items":["/logs/stream","/metrics/stream"]}',
    );
  });

  it("leaves a multi-key object alone even when one key is v", () => {
    expect(factValue({ v: 1, unit: "ms" })).toBe('{"v":1,"unit":"ms"}');
  });

  it("renders an empty value without throwing", () => {
    expect(factValue({})).toBe("{}");
  });
});
