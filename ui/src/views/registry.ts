export const VIEWS = [
  { id: "now", label: "Now" },
  { id: "trace", label: "Trace" },
  { id: "memory", label: "Memory" },
  { id: "shape", label: "Shape" },
  { id: "constellation", label: "Constellation" },
  { id: "signals", label: "Signals" },
] as const;

export type ViewId = (typeof VIEWS)[number]["id"];

export function isViewId(value: string): value is ViewId {
  return VIEWS.some((view) => view.id === value);
}
