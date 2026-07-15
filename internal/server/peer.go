package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// SelfPeer is one live peer in the authenticated agent's realm. Activity is
// optional because agents created before activity hooks (or agents that have
// never run a client) have no observation yet.
type SelfPeer struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
	LastRuntime    string     `json:"last_runtime,omitempty"`
	LastLocation   string     `json:"last_location,omitempty"`
	LastEvent      string     `json:"last_event,omitempty"`
}

// SelfPeers is the bounded response from GET /v1/self/peers.
type SelfPeers struct {
	SchemaVersion string     `json:"schema_version"`
	Peers         []SelfPeer `json:"peers"`
}

func selfPeersHandler(
	auth PrincipalAuthFunc,
	list func(context.Context, DomainPrincipal) ([]SelfPeer, error),
) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		w.Header().Set("Cache-Control", "private, no-store")
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may list peers")
			return
		}
		if r.URL.RawQuery != "" {
			writeJSONError(w, http.StatusBadRequest, "peer listing does not accept query parameters")
			return
		}

		peers, err := list(r.Context(), p)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not list peers")
			return
		}
		// Defense in depth for embedders: the HTTP contract never exposes the
		// caller as its own peer even if a callback accidentally includes it.
		filtered := make([]SelfPeer, 0, len(peers))
		for _, peer := range peers {
			if peer.ID != p.ID {
				filtered = append(filtered, peer)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SelfPeers{
			SchemaVersion: "witself.v0",
			Peers:         filtered,
		})
	})
}
