import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { ScopeFilter } from "@/components/memory/ScopeFilter";
import { createStubTransport, renderWithProviders } from "@/test/render";
import { useProjects } from "@/api/queries";
import type { Project, ProjectCounts } from "@/api/types";

function project(slug: string, counts: Partial<ProjectCounts>): Project {
  return {
    id: `id-${slug}`,
    slug,
    user: "default",
    created_at: "2026-08-01T00:00:00Z",
    last_activity: null,
    counts: { facts: 0, experiences: 0, skills: 0, entities: 0, sources: 0, ...counts },
  };
}

function renderFilter(projects: Project[]) {
  return renderWithProviders(<ScopeFilter selected={null} onChange={() => undefined} />, {
    transport: createStubTransport({
      responses: { "/v1/projects": { items: projects, next_cursor: null } },
    }),
  });
}

describe("ScopeFilter", () => {
  it("offers a project that has memory stored in it", async () => {
    renderFilter([project("anamnesia-ui", { facts: 12, sources: 30 })]);

    expect(await screen.findByRole("button", { name: /anamnesia-ui/ })).toBeInTheDocument();
  });

  /**
   * A project that was ingested but never extracted has sources and nothing
   * else. Filtering to it shows empty tables in every domain, so it is noise.
   */
  it("hides a project that has sources but nothing extracted from them", async () => {
    renderFilter([
      project("anamnesia-ui", { facts: 12 }),
      project("ingest-only", { sources: 5 }),
    ]);

    await screen.findByRole("button", { name: /anamnesia-ui/ });
    expect(screen.queryByRole("button", { name: /ingest-only/ })).not.toBeInTheDocument();
  });

  it("hides a project with nothing stored at all", async () => {
    renderFilter([project("anamnesia-ui", { experiences: 3 }), project("brand-new", {})]);

    await screen.findByRole("button", { name: /anamnesia-ui/ });
    expect(screen.queryByRole("button", { name: /brand-new/ })).not.toBeInTheDocument();
  });

  it("counts experiences as memory, not just facts", async () => {
    renderFilter([project("only-experiences", { experiences: 4, sources: 9 })]);

    expect(await screen.findByRole("button", { name: /only-experiences/ })).toBeInTheDocument();
  });

  /**
   * A lone "all projects" chip offers no choice, so the row goes entirely.
   *
   * The probe shares the query client, so it turns "loaded" only once the
   * directory has arrived. Without it this asserts against the loading state,
   * where the filter renders nothing anyway, and would pass on any code.
   */
  it("renders nothing when no project qualifies", async () => {
    function Probe() {
      const projects = useProjects();
      return <span data-testid="probe">{projects.isSuccess ? "loaded" : "loading"}</span>;
    }

    renderWithProviders(
      <>
        <ScopeFilter selected={null} onChange={() => undefined} />
        <Probe />
      </>,
      {
        transport: createStubTransport({
          responses: {
            "/v1/projects": {
              items: [project("ingest-only", { sources: 5 }), project("brand-new", {})],
              next_cursor: null,
            },
          },
        }),
      },
    );

    await waitFor(() => expect(screen.getByTestId("probe")).toHaveTextContent("loaded"));
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });
});
