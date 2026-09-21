import "@testing-library/jest-dom/vitest";

/**
 * jsdom has no 2D canvas, and calling getContext on it prints a paragraph of
 * "Not implemented" for every figure that draws. Answering null is the truth,
 * and every drawing component already treats a missing context as "there is
 * nothing to draw on" rather than crashing.
 */
HTMLCanvasElement.prototype.getContext = () => null;
