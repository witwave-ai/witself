// Module-resolution hook that lets plain Node import src/index.js: the
// worker's only workerd-scheme dependency is @cloudflare/containers, which
// this hook redirects to the local hermetic stub. Registered from the
// signup-lifecycle boundary tests via node:module register() before they
// dynamically import the worker.
const STUB_URL = new URL("./cloudflare-containers-stub.mjs", import.meta.url).href;

export async function resolve(specifier, context, nextResolve) {
  if (specifier === "@cloudflare/containers") {
    return { shortCircuit: true, url: STUB_URL };
  }
  return nextResolve(specifier, context);
}
