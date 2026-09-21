export type Vec3 = readonly [number, number, number];

export interface Icosphere {
  vertices: Vec3[];
  edges: Array<readonly [Vec3, Vec3]>;
  faces: number;
}

const PHI = (1 + Math.sqrt(5)) / 2;

const unit = (v: Vec3): Vec3 => {
  const m = Math.hypot(v[0], v[1], v[2]) || 1;
  return [v[0] / m, v[1] / m, v[2] / m];
};

const midpoint = (a: Vec3, b: Vec3): Vec3 =>
  unit([(a[0] + b[0]) / 2, (a[1] + b[1]) / 2, (a[2] + b[2]) / 2]);

const key = (v: Vec3) => v.map((n) => n.toFixed(5)).join(",");

/**
 * The die at the centre of the constellation.
 *
 * An icosahedron subdivided toward a sphere: at zero it is the twenty-sided
 * solid everyone recognises, and each pass quarters every face. Two passes
 * give 320 faces, which is the point where the cage reads as a faceted body
 * rather than as a wireframe ball or a coarse cage.
 */
export function icosphere(subdivisions: number): Icosphere {
  const seed: Vec3[] = [];
  for (const a of [-1, 1]) {
    for (const b of [-1, 1]) {
      seed.push([0, a, b * PHI], [a, b * PHI, 0], [b * PHI, 0, a]);
    }
  }
  const corners = seed.map(unit);

  // The twenty faces are the triples of mutually adjacent corners. On the unit
  // icosahedron every edge is the same length, so adjacency is a distance test.
  const adjacent = (a: Vec3, b: Vec3) =>
    Math.hypot(a[0] - b[0], a[1] - b[1], a[2] - b[2]) < 1.12;

  let faces: Array<[Vec3, Vec3, Vec3]> = [];
  for (let i = 0; i < corners.length; i += 1) {
    for (let j = i + 1; j < corners.length; j += 1) {
      for (let k = j + 1; k < corners.length; k += 1) {
        const [a, b, c] = [corners[i]!, corners[j]!, corners[k]!];
        if (adjacent(a, b) && adjacent(b, c) && adjacent(a, c)) faces.push([a, b, c]);
      }
    }
  }

  for (let pass = 0; pass < subdivisions; pass += 1) {
    faces = faces.flatMap(([a, b, c]) => {
      const ab = midpoint(a, b);
      const bc = midpoint(b, c);
      const ca = midpoint(c, a);
      return [
        [a, ab, ca],
        [ab, b, bc],
        [ca, bc, c],
        [ab, bc, ca],
      ] as Array<[Vec3, Vec3, Vec3]>;
    });
  }

  const vertices = new Map<string, Vec3>();
  const edges = new Map<string, readonly [Vec3, Vec3]>();
  for (const face of faces) {
    for (const v of face) vertices.set(key(v), v);
    for (const [a, b] of [
      [face[0], face[1]],
      [face[1], face[2]],
      [face[2], face[0]],
    ] as Array<[Vec3, Vec3]>) {
      edges.set([key(a), key(b)].sort().join("|"), [a, b]);
    }
  }

  return { vertices: [...vertices.values()], edges: [...edges.values()], faces: faces.length };
}
