import { describe, expect, it } from "vitest";

import { icosphere } from "@/components/constellation/die";

describe("icosphere", () => {
  it("starts as the twenty-sided solid a d20 actually is", () => {
    const { vertices, edges } = icosphere(0);

    expect(vertices).toHaveLength(12);
    expect(edges).toHaveLength(30);
  });

  /** V - E + F = 2 for any convex polyhedron, so this catches a bad split. */
  it("stays a closed solid through two subdivisions", () => {
    const { vertices, edges, faces } = icosphere(2);

    expect(vertices).toHaveLength(162);
    expect(edges).toHaveLength(480);
    expect(faces).toBe(320);
    expect(vertices.length - edges.length + faces).toBe(2);
  });

  it("keeps every vertex on the unit sphere, so the cage reads as round", () => {
    const { vertices } = icosphere(2);

    for (const [x, y, z] of vertices) {
      expect(Math.hypot(x, y, z)).toBeCloseTo(1, 6);
    }
  });
});
