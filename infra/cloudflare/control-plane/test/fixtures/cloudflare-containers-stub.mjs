// Hermetic stand-in for @cloudflare/containers. The real package imports
// the "cloudflare:workers" scheme, which only exists inside workerd, so the
// signup-lifecycle boundary tests resolve this stub instead (see
// cloudflare-containers-loader.mjs). The stub records every cold-path
// container fetch so tests can prove lifecycle routes never fall through
// to the Go container.
export class Container {}

export const containerCalls = [];

export function resetContainerCalls() {
  containerCalls.length = 0;
}

export function getContainer() {
  return {
    async fetch(request) {
      containerCalls.push(`${request.method} ${new URL(request.url).pathname}`);
      return new Response(
        JSON.stringify({ schema_version: "witself.v0", error: "cold path stub" }),
        { status: 599, headers: { "Content-Type": "application/json" } },
      );
    },
  };
}
