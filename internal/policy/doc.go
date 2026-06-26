// Package policy is the open-plane policy engine. It owns evaluable Policy
// objects (subject x permission x target x scope), the default-deny stance, the
// escalating verbs (read, contribute, curate, forget), and the `policy test`
// decision path. It governs cross-agent identity access to memories/facts only;
// sealed-plane secret access uses grants plus realm roles, not policy verbs.
package policy
