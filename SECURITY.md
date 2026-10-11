# Security Policy

Witself stores and brokers access to agent identity data (memories, facts,
cross-agent access policy, security groups and inter-agent messages) and to
sealed-plane secret material that clients encrypt. Please report security
issues privately.

Please send suspected vulnerabilities to `security@witwave.ai`.

Do not include live customer identity data, memory content, fact values, message
bodies or payloads, personal data or other PII, embedding vectors, raw tokens,
payment details, wallet credentials, or private keys unless we explicitly ask
for them through a secure channel.

Both security postures are in scope: the integrity and authenticity of
open-plane identity data, and the confidentiality of sealed-plane secret
material. Cross-agent access bypass (acting on another agent's memories or
facts without a permitting policy) and message spoofing (forging the `from`
agent on an inter-agent message) are explicitly in scope, alongside
memory-poisoning, unauthorized curation or forgetting, and cross-agent write
abuse. On the sealed plane, secret-value leakage or a reveal bypass is in
scope.

On Witself Cloud the account email is the root of trust: whoever controls that
inbox can recover the owner credential. See
[docs/security-policy.md](docs/security-policy.md#account-email-is-the-root-of-trust).

The full policy is [docs/security-policy.md](docs/security-policy.md). The
threat model is [docs/threat-model.md](docs/threat-model.md).
