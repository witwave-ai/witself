package main

const factCapacityMCPRoutingInstructions = `## Witself current-fact capacity

- Treat the authenticated, value-free fact_capacity from self.show, or the equivalent advertised fact-status read, as the current count and policy state for resolved, non-deleted facts across this agent's subjects. Near limit begins at 90 percent of a finite maximum; unlimited capacity needs no pressure response.
- At or above the maximum, reads, history, export, permanent deletion under its separate direct-user authorization rule, and updates to an already-current fact remain available. Do not retry a stored_fact_limit_reached refusal for the same count-growing intent. Creating or recreating another current fact, including confirmation into a new fact address, must wait for capacity or a policy change.
- Fact capacity is guidance, never deletion authority. Do not permanently delete or rewrite an unrelated fact merely to make room, and do not infer unlimited capacity when fact_capacity is unavailable.`
