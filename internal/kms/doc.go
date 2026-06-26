// Package kms is the sealed-plane KMS-provider abstraction that roots the
// envelope: the aws-kms, gcp-kms, azure-key-vault, and local-dev providers
// behind a capability boundary, supporting client-side and server-side decrypt.
// It is required only when the sealed plane is enabled.
package kms
