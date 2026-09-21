import { fileURLToPath, URL } from "node:url";

import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

const DEFAULT_ANAMNESIA_URL = "http://host.docker.internal:8181";

/**
 * Where to proxy `/api`.
 *
 * An unset variable and an empty one mean the same thing here. They did not
 * always: `make dev` passes `-e ANAMNESIA_URL=` when the caller has not set
 * one, which Node reports as an empty string rather than undefined, so a `??`
 * fallback did not fire and the proxy was handed `""`. http-proxy then read a
 * protocol off nothing and took the whole dev server down on the first
 * request, with a stack trace that named neither the variable nor the cause.
 */
function anamnesiaUrl(): string {
  const raw = process.env["ANAMNESIA_URL"]?.trim();
  if (!raw) return DEFAULT_ANAMNESIA_URL;

  try {
    // Fail at startup with something readable, rather than per-request with a
    // parser error from inside a dependency.
    new URL(raw);
  } catch {
    throw new Error(
      `ANAMNESIA_URL is set to "${raw}", which is not a URL. Expected something like ${DEFAULT_ANAMNESIA_URL}.`,
    );
  }
  return raw;
}

// The dev server runs inside a container, so it must bind every interface for
// the host browser to reach it. `/api` is proxied to `anamnesia serve` so the
// browser never needs CORS and never sees the server token.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  // The bundle is embedded into the Go binary, and `go:embed` cannot reach
  // outside the directory of the package that embeds it, so the build writes
  // straight into that package rather than into a local dist/.
  build: {
    outDir: fileURLToPath(new URL("../internal/ui/dist", import.meta.url)),
    emptyOutDir: true,
  },
  server: {
    host: "0.0.0.0",
    port: 5173,
    proxy: {
      "/api": {
        target: anamnesiaUrl(),
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/api/, ""),
        // A proxy error must not take the dev server down with it. The
        // server being unreachable is the normal case before it is started.
        configure: (proxy) => {
          proxy.on("error", (error, _req, res) => {
            const message = `Cannot reach anamnesia at ${anamnesiaUrl()}: ${error.message}`;
            if ("writeHead" in res && !res.headersSent) {
              res.writeHead(502, { "Content-Type": "text/plain" });
            }
            if ("end" in res) res.end(message);
          });
        },
      },
    },
  },
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    css: false,
  },
});
