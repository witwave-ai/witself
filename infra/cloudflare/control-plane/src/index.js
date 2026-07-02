// Thin Worker front door for witself-control-plane.
//
// HOT PATH (this file): the account->cell directory is answered entirely at the
// edge from the DIRECTORY KV binding — it never touches the container. Per the
// 2026-07 scaling research: Workers+KV reads are effectively unbounded (hot keys
// <1ms from PoP cache), while containers are a fixed instance count with a ~1k
// rps Durable Object cap each and 1-3s cold starts. This split is what makes the
// control plane "scale from zero": the container can be asleep and directory
// lookups still serve globally at full speed.
//
// COLD PATH: everything else forwards to the Go container (signup, webhooks,
// cell registry — all later slices).
//
// KV schema (v0): "acct:<account_id>" -> {"cell":"<name>","endpoint":"https://..."}
// Writes happen out-of-band for now (wrangler kv / the future registration
// slice); the directory is a read projection, never read-modify-written here.
// cacheTtl 300s: directory entries change rarely (placement, migration), and
// cells must tolerate briefly-stale routing by redirecting misrouted requests.
//
// NOTE: lookups are unauthenticated in v0 — account ids are unguessable 80-bit
// random handles and the mapping is low-sensitivity routing metadata. Revisit
// when tokens reach the control plane.
import { Container, getContainer } from "@cloudflare/containers";

export class ControlPlane extends Container {
  defaultPort = 8080;
  sleepAfter = "10m";
}

const json = (obj, status = 200, extra = {}) =>
  new Response(JSON.stringify(obj), {
    status,
    headers: { "Content-Type": "application/json", ...extra },
  });

const DIRECTORY_PATH = /^\/v1\/directory\/([A-Za-z0-9_-]{1,128})$/;

export default {
  async fetch(request, env) {
    const url = new URL(request.url);

    const m = url.pathname.match(DIRECTORY_PATH);
    if (m) {
      if (request.method !== "GET") {
        return json({ schema_version: "witself.v0", error: "method not allowed" }, 405);
      }
      const entry = await env.DIRECTORY.get(`acct:${m[1]}`, {
        type: "json",
        cacheTtl: 300,
      });
      if (!entry) {
        return json({ schema_version: "witself.v0", error: "unknown account" }, 404);
      }
      return json(
        { schema_version: "witself.v0", account_id: m[1], cell: entry },
        200,
        // Clients (the ws CLI) are expected to cache their answer with a TTL
        // and stale-on-error — that, not this header, is the primary lever.
        { "Cache-Control": "max-age=60" },
      );
    }

    return getContainer(env.CONTROL_PLANE, "singleton").fetch(request);
  },
};
