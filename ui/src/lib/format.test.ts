import { describe, expect, it } from "vitest";

import {
  formatBytes,
  formatDuration,
  formatInterval,
  formatRelative,
  formatUptime,
  groupThousands,
  joinMeta,
  truncate,
} from "@/lib/format";

describe("formatDuration", () => {
  it("keeps sub-second values in milliseconds", () => {
    expect(formatDuration(37)).toBe("37 ms");
  });

  it("groups thousands rather than switching units too early", () => {
    expect(formatDuration(1840)).toBe("1\u2009840 ms");
  });

  it("switches to seconds once milliseconds stop being readable", () => {
    expect(formatDuration(6204)).toBe("6.2 s");
  });

  it("does not render a negative or non-finite duration as a number", () => {
    expect(formatDuration(-1)).toBe("-");
    expect(formatDuration(Number.NaN)).toBe("-");
  });
});

describe("formatRelative", () => {
  const now = new Date("2026-08-18T12:00:00Z");

  it.each([
    ["2026-08-18T11:59:48Z", "12s ago"],
    ["2026-08-18T11:38:00Z", "22m ago"],
    ["2026-08-18T06:00:00Z", "6h ago"],
    ["2026-08-15T12:00:00Z", "3d ago"],
  ])("renders %s as %s", (when, expected) => {
    expect(formatRelative(when, now)).toBe(expected);
  });

  it("does not report a future timestamp as negative", () => {
    expect(formatRelative("2026-08-18T12:00:05Z", now)).toBe("just now");
  });
});

describe("formatUptime", () => {
  it.each([
    [45_000, "45s"],
    [2_040_000, "34m"],
    [21_600_000, "6h"],
    [259_200_000, "3d"],
  ])("renders %ims as %s", (ms, expected) => {
    expect(formatUptime(ms)).toBe(expected);
  });
});

describe("formatInterval", () => {
  it.each([
    [1000, "every 1s"],
    [60_000, "every 1m"],
    [86_400_000, "every 24h"],
  ])("renders %ims as %s", (ms, expected) => {
    expect(formatInterval(ms)).toBe(expected);
  });
});

describe("misc formatting", () => {
  it("formats bytes in decimal units, matching the CLI", () => {
    expect(formatBytes(4271)).toBe("4.3 kB");
  });

  it("groups thousands with a thin space, never a comma", () => {
    expect(groupThousands(8123)).toBe("8\u2009123");
  });

  it("drops empty parts when joining meta", () => {
    expect(joinMeta("v0.1.0", null, "", "34m uptime")).toBe("v0.1.0 · 34m uptime");
  });

  it("truncates on a word boundary", () => {
    expect(truncate("anamnesia-open-source", 17)).toBe("anamnesia-open-s\u2026");
    expect(truncate("anamnesia-open-source", 17)).toHaveLength(17);
    expect(truncate("short", 17)).toBe("short");
  });
});
