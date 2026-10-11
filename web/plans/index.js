// witself-plans: serves the public Witself plan catalog as JSON.
//
// The catalog lives in plans.json next to this file — git is the source of
// truth; `wrangler deploy` publishes it. Plans with "available": false (today
// only Enterprise) are announced but not purchasable. Keys in
// coming_soon_features stay in each plan's features, the resolved entitlement
// the control plane pushes to cells, but are not yet delivered. Every public
// catalog surface shows them as coming soon.
import plans from "./plans.json";

const body = JSON.stringify(plans, null, 2) + "\n";

export default {
  fetch(request) {
    if (request.method !== "GET" && request.method !== "HEAD") {
      return new Response("method not allowed\n", { status: 405, headers: { allow: "GET, HEAD" } });
    }
    return new Response(body, {
      headers: {
        "content-type": "application/json; charset=utf-8",
        // Public data; let the pricing page and clients fetch it from anywhere.
        "access-control-allow-origin": "*",
        // Short cache so a deploy propagates within minutes.
        "cache-control": "public, max-age=300",
      },
    });
  },
};
