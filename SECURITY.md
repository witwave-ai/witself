# Security Policy

Witself is being designed to store and broker access to agent self/identity
material: memories, facts, cross-agent access policy, security groups, and
inter-agent messages. Security reports should be handled privately.

Please send suspected vulnerabilities to `security@witwave.ai`.

Do not include live customer identity data, memory content, fact values, message
bodies or payloads, personal data or other PII, embedding vectors, raw tokens,
payment details, wallet credentials, or private keys unless we explicitly ask
for them through a secure channel.

Witself's threat model is integrity and authenticity of identity data, not
secret confidentiality. Cross-agent access bypass (acting on another agent's
memories or facts without a permitting policy) and message spoofing (forging the
`from` agent on an inter-agent message) are explicitly in scope, alongside
memory-poisoning, unauthorized curation or forgetting, and cross-agent write
abuse.

The detailed draft security policy is in
[docs/security-policy.md](docs/security-policy.md). The current draft threat
model is in [docs/threat-model.md](docs/threat-model.md).
