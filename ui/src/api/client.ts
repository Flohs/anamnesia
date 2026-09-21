import { createHttpTransport, type Transport } from "@/api/transport";

/**
 * Builds the transport the console talks through.
 *
 * There is one: HTTP to `anamnesia serve`, via the same-origin `/api` prefix
 * that the dev server and the production server both proxy. Tests inject their
 * own through TransportProvider rather than going through here.
 */
export function createTransport(): Transport {
  return createHttpTransport();
}
