// Thin Worker front door for witself-control-plane.
//
// Today it forwards everything to the Go container (bare-bones slice). As the
// control plane grows, the HOT PATH (account->cell directory lookups) is
// answered here from a KV binding — Workers+KV scale effectively without
// ceiling, while the container path is capped (~1k rps per fixed instance, no
// autoscaling as of 2026-07). Only cold-path work (signup, webhooks, cell
// registry) should ever reach the container.
import { Container, getContainer } from "@cloudflare/containers";

export class ControlPlane extends Container {
  defaultPort = 8080;
  sleepAfter = "10m";
}

export default {
  async fetch(request, env) {
    return getContainer(env.CONTROL_PLANE, "singleton").fetch(request);
  },
};
