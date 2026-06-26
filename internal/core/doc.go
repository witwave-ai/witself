// Package core holds the domain service and use cases behind the thin CLI, MCP,
// and API adapters. It spans both planes: the open plane
// (memory/fact/policy/group/message) and the sealed plane (secret/totp/grant),
// so behavior never drifts across adapters.
package core
