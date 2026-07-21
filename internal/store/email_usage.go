package store

// Agent-email usage has a separate namespace from realm-local messaging.
// These names are the canonical join keys shared by usage reports, capability
// limits, and billing-provider adapters. The Cloudflare receive pilot must not
// emit any of these as billable usage: it lacks authoritative spam and abuse
// classification, so charging from pilot ingestion would let hostile senders
// bill the recipient.
const (
	UsageDimensionEmailReceived = "email_received"
	UsageDimensionEmailSent     = "email_sent"
	UsageDimensionEmailAddress  = "email_address"
	UsageDimensionEmailStorage  = "email_storage_byte"

	UsageUnitEmail        = "email"
	UsageUnitEmailAddress = "address"
)
