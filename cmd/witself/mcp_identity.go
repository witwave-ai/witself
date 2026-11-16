package main

import "github.com/witwave-ai/witself/internal/memoryhydration"

type mcpIdentityForm = memoryhydration.IdentityForm

const (
	mcpIdentityShort   = memoryhydration.IdentityShort
	mcpIdentityOneLine = memoryhydration.IdentityOneLine
)

func renderMCPIdentityParagraph(agent, owner string, form mcpIdentityForm) string {
	return memoryhydration.RenderIdentityStatement(agent, owner, form)
}

func validMCPIdentityName(name string) bool {
	return memoryhydration.ValidIdentityName(name)
}
