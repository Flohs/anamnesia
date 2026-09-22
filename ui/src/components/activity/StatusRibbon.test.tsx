import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { StatusRibbon } from "@/components/activity/StatusRibbon";
import { renderWithProviders } from "@/test/render";
import type { Health, RecallCounts } from "@/api/types";

function health(overrides: Partial<Health> = {}): Health {
  return {
    ok: true,
    service: "anamnesia",
    version: "0.1.0",
    database: "ok",
    migration_version: 12,
    schema_embed_dims: 1536,
    configured_embed_dims: 1536,
    embed_model: "openai/text-embedding-3-small",
    llm_provider: "openrouter",
    llm_model: "openai/gpt-4o-mini",
    ...overrides,
  };
}

function ribbon(recall: RecallCounts | null) {
  return (
    <StatusRibbon
      health={health()}
      startedAt={new Date().toISOString()}
      projectCount={3}
      experienceCount={205}
      factCount={294}
      entityCount={12}
      user="default"
      recall={recall}
    />
  );
}

describe("StatusRibbon recall", () => {
  it("counts what was recalled against the prompts that could be judged", () => {
    renderWithProviders(ribbon({ days: 7, prompts: 640, recalled: 412, ungraded: 0 }));

    expect(screen.getByText("412 recalled")).toBeInTheDocument();
    // Short enough to survive the cell's truncation at seven across.
    expect(screen.getByText("640 prompts · 7 days")).toBeInTheDocument();
  });

  // An ungraded prompt had nothing absolute to judge it by, so counting
  // it as a prompt that failed to recall would invent a failure.
  it("leaves ungraded prompts out of the count it is measured against", () => {
    renderWithProviders(ribbon({ days: 7, prompts: 100, recalled: 30, ungraded: 40 }));

    expect(screen.getByText("30 recalled")).toBeInTheDocument();
    expect(screen.getByText("60 prompts · 7 days")).toBeInTheDocument();
  });

  // The lexical-only install: every prompt is ungraded, and reporting
  // "0 recalled" would read as broken retrieval rather than as a setup
  // with no embedder to measure against.
  it("says it was not measured rather than showing a zero it cannot stand behind", () => {
    renderWithProviders(ribbon({ days: 7, prompts: 80, recalled: 0, ungraded: 80 }));

    expect(screen.getByText("not measured")).toBeInTheDocument();
    expect(screen.getByText(/80 prompts/)).toBeInTheDocument();
    expect(screen.queryByText("0 recalled")).not.toBeInTheDocument();
  });

  it("says nothing has been asked yet on an install with no prompts in the window", () => {
    renderWithProviders(ribbon({ days: 7, prompts: 0, recalled: 0, ungraded: 0 }));

    expect(screen.getByText("no prompts yet")).toBeInTheDocument();
  });

  it("waits rather than guessing while the tally is still loading", () => {
    renderWithProviders(ribbon(null));

    expect(screen.getByText("recall")).toBeInTheDocument();
    expect(screen.queryByText(/recalled/)).not.toBeInTheDocument();
  });

  it("still shows the six cells it always had", () => {
    renderWithProviders(ribbon({ days: 7, prompts: 10, recalled: 5, ungraded: 0 }));

    for (const label of ["server", "database", "model", "embeddings", "scope", "stored"]) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
  });
});
