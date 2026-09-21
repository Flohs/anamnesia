import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/**
 * Joins class names and lets a later conflicting utility win.
 *
 * Without the merge, `cn("p-2", "p-4")` emits both and the winner depends on
 * stylesheet order, which is how component props stop overriding defaults.
 */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
