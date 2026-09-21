import { useEffect, useMemo, useRef } from "react";

import { domainHue } from "@/components/constellation/hues";
import { icosphere, type Vec3 } from "@/components/constellation/die";

export interface CloudPoint {
  x: number;
  y: number;
  /** The domain the record belongs to, plural, as the stats report it. */
  domain: string;
}

interface MemoryDieProps {
  cloud: readonly CloudPoint[];
  /** When set, every other domain dims rather than disappearing. */
  isolate?: string | null;
}

const TAU = Math.PI * 2;
const GOLDEN = (1 + Math.sqrt(5)) / 2;
const FOV = 3.2;

/**
 * The store itself, at the centre of the ring.
 *
 * A slowly turning many-sided die holding one point per embedded record. The
 * two axes are the server's own projection; the third is a spiral that keeps
 * the cloud a body rather than a disc and carries no meaning, which the view
 * says out loud rather than implying depth it does not have.
 */
export function MemoryDie({ cloud, isolate = null }: MemoryDieProps) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const cage = useMemo(() => icosphere(2), []);

  const points = useMemo(() => {
    if (cloud.length === 0) return [];
    const xs = cloud.map((p) => p.x);
    const ys = cloud.map((p) => p.y);
    const span = (values: number[]) => {
      const low = Math.min(...values);
      return { low, width: Math.max(...values) - low || 1 };
    };
    const x = span(xs);
    const y = span(ys);

    return cloud.map((p, i) => ({
      v: [
        ((p.x - x.low) / x.width) * 2 - 1,
        ((p.y - y.low) / y.width) * 2 - 1,
        Math.sin((i / cloud.length) * TAU * GOLDEN) * 0.55,
      ] as Vec3,
      hue: domainHue(p.domain),
      domain: p.domain,
    }));
  }, [cloud]);

  // The isolate lives in a ref so changing it never restarts the animation.
  const isolated = useRef(isolate);
  isolated.current = isolate;

  useEffect(() => {
    const canvas = canvasRef.current;
    const ctx = canvas?.getContext("2d");
    if (!canvas || !ctx) return;

    const reduced = matchMedia("(prefers-reduced-motion: reduce)").matches;
    let width = 0;
    let height = 0;
    let frame = 0;
    let spin = 0;
    let last = 0;

    const measure = () => {
      const box = canvas.getBoundingClientRect();
      const dpr = Math.min(devicePixelRatio || 1, 2);
      width = Math.max(box.width, 1);
      height = Math.max(box.height, 1);
      canvas.width = width * dpr;
      canvas.height = height * dpr;
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    };

    const spun = (v: Vec3, ax: number, ay: number): Vec3 => {
      let [x, y, z] = v;
      let c = Math.cos(ay);
      let s = Math.sin(ay);
      [x, z] = [x * c + z * s, z * c - x * s];
      c = Math.cos(ax);
      s = Math.sin(ax);
      [y, z] = [y * c - z * s, z * c + y * s];
      return [x, y, z];
    };

    const draw = (now: number) => {
      const delta = last ? Math.min(now - last, 60) : 0;
      last = now;
      if (!reduced) spin += delta * 0.00008;

      const cx = width / 2;
      const cy = height / 2;
      const radius = Math.min(width, height) / 2;
      const project = (v: Vec3, scale: number) => {
        const k = FOV / (FOV + v[2]);
        return { x: cx + v[0] * scale * k, y: cy + v[1] * scale * k, k };
      };

      ctx.clearRect(0, 0, width, height);
      const ax = Math.sin(spin * 0.6) * 0.42;
      const shell = radius * 0.55;
      const inner = radius * 0.24;

      ctx.lineWidth = 1;
      for (const [a, b] of cage.edges) {
        const p = project(spun(a, ax, spin), shell);
        const q = project(spun(b, ax, spin), shell);
        ctx.beginPath();
        ctx.moveTo(p.x, p.y);
        ctx.lineTo(q.x, q.y);
        ctx.strokeStyle = `rgba(214,214,228,${0.012 + Math.max(p.k - 0.8, 0) * 0.24})`;
        ctx.stroke();
      }

      // Far points first, so the near face of the cloud sits on top.
      const drawn = points
        .map((p) => ({ p, at: project(spun(p.v, ax, spin), inner) }))
        .sort((one, other) => one.at.k - other.at.k);

      for (const { p, at } of drawn) {
        const dim = isolated.current !== null && p.domain !== isolated.current;
        ctx.beginPath();
        ctx.arc(at.x, at.y, Math.max(0.8, 1.9 * at.k), 0, TAU);
        ctx.fillStyle = p.hue;
        ctx.globalAlpha = dim ? 0.05 : 0.32 + (at.k - 0.7) * 1.5;
        ctx.fill();
      }
      ctx.globalAlpha = 1;

      frame = requestAnimationFrame(draw);
    };

    measure();
    frame = requestAnimationFrame(draw);
    const observer = new ResizeObserver(measure);
    observer.observe(canvas);

    return () => {
      cancelAnimationFrame(frame);
      observer.disconnect();
    };
  }, [cage, points]);

  return (
    <canvas
      ref={canvasRef}
      className="absolute inset-0 size-full"
      role="img"
      aria-label={`${cloud.length} embedded memories, turning inside a many-sided die`}
    />
  );
}
