package main

import "strings"

// tenantRowsF2 deliberately spells out every pattern. Neither this table nor
// its helpers read the inventory: a newly mounted route must get a reviewed row.
// Foreign template tokens use the requested side; own.KEY selects an A-owned prerequisite.
func tenantRowsF2() []tenantRow {
	return []tenantRow{
		f2Foreign("GET /v1/agents/{agent}/email-receive", "agent", credOperator, "/v1/agents/${agent}/email-receive", ""),
		f2Foreign("PATCH /v1/agents/{agent}/email-receive", "agent", credOperator, "/v1/agents/${agent_email_receive}/email-receive", `{"receive_state":"enabled"}`),
		f2Foreign("GET /v1/realms/{realm}/email-receive", "realm", credOperator, "/v1/realms/${realm}/email-receive", ""),
		f2Foreign("PATCH /v1/realms/{realm}/email-receive", "realm", credOperator, "/v1/realms/${realm_email_receive}/email-receive", `{"receive_state":"enabled"}`),
		f2Foreign("GET /v1/agents/{agent}/email-send", "agent", credOperator, "/v1/agents/${agent}/email-send", ""),
		f2Foreign("PATCH /v1/agents/{agent}/email-send", "agent", credOperator, "/v1/agents/${agent_email_send}/email-send", `{"send_state":"enabled","expected_row_version":1}`),
		f2Foreign("GET /v1/realms/{realm}/email-send", "realm", credOperator, "/v1/realms/${realm}/email-send", ""),
		f2Foreign("PATCH /v1/realms/{realm}/email-send", "realm", credOperator, "/v1/realms/${realm_email_send}/email-send", `{"send_state":"enabled","expected_row_version":1}`),

		f2OwnFresh("POST /v1/vault/key-epochs", "Registers a new client-generated key for the authenticated agent; no existing foreign object is selected (secret.go:57-64).", credAgent, "vault_register", `{"id":"${vault_key_new}","key_version":1,"algorithm":"AES_256_GCM_RANDOM_NONCE_V1","fingerprint":"${vault_fingerprint}"}`),
		f2Own("GET /v1/vault/key-epochs/current", "Current key is selected solely by the full agent principal (secret.go:286).", credAgent, "", false),
		f2OwnFresh("POST /v1/vault/enrollments", "Creates a new enrollment against the caller's current vault key; location and public key are client-generated metadata (vault_enrollment.go:51-60).", credAgent, "enrollment_create", `{"id":"${enrollment_new}","target_location_id":"${location_id}","target_location_name":"${location_name}","target_public_key":"${vault_public_key}","target_key_algorithm":"X25519_RAW_32_BASE64URL_V1","pairing_commitment":"${pairing_commitment}","expires_at":"${enrollment_expiry}"}`),
		f2Own("GET /v1/vault/enrollments", "Lists only the caller's enrollments; no owner selector is accepted (vault_enrollment.go:217).", credAgent, "", true),
		f2Foreign("GET /v1/vault/enrollments/{enrollment}", "enrollment", credAgent, "/v1/vault/enrollments/${enrollment}", ""),
		{
			Pattern: "POST /v1/vault/enrollments/{action}", Class: tenant,
			Handler: "vaultKeyEnrollmentActionHandler", Actions: []string{"approve", "receive", "consume", "cancel"}, // vault_enrollment.go:123-185.
			Probes: []tenantProbe{
				f2FreshProbe("approve", credAgent, "POST", "/v1/vault/enrollments/${enrollment_approve}:approve", "enrollment_approve", `{"expected_row_version":1,"source_location_id":"${source_location_id}","source_ephemeral_public_key":"${vault_public_key}","transfer_ciphertext":"${vault_transfer}","transfer_algorithm":"X25519_HKDF_SHA256_AES_256_GCM_V1","consume_commitment":"${consume_commitment}"}`),
				f2FreshProbe("receive", credAgent, "POST", "/v1/vault/enrollments/${enrollment_receive}:receive", "enrollment_receive", `{"target_location_id":"${location_id}"}`),
				f2BodyProbe("receive-location-body", credAgent, "POST", "/v1/vault/enrollments/${enrollment_receive}:receive", "enrollment_receive_location-body", `{"target_location_id":"${location_id}"}`, "location_id", "target_location_id"),
				f2FreshProbe("consume", credAgent, "POST", "/v1/vault/enrollments/${enrollment_consume}:consume", "enrollment_consume", `{"expected_row_version":2,"target_location_id":"${location_id}","consume_proof":"${consume_proof}"}`),
				f2Refusal(f2BodyProbe("consume-location-body", credAgent, "POST", "/v1/vault/enrollments/${enrollment_consume}:consume", "enrollment_consume_location-body", `{"expected_row_version":2,"target_location_id":"${location_id}","consume_proof":"${consume_proof}"}`, "location_id", "target_location_id"), 409, "internal/store/vault_enrollment.go:465-467 compares the caller-owned enrollment location; internal/server/secret.go:652-653 maps conflict to 409."),
				f2FreshProbe("cancel", credAgent, "POST", "/v1/vault/enrollments/${enrollment_cancel}:cancel", "enrollment_cancel", `{"expected_row_version":1}`),
			},
		},
		{
			Pattern: "POST /v1/vault/rotations", Class: tenant,
			Probes: []tenantProbe{f2Refusal(f2BodyProbe("source-key", credAgent, "POST", "/v1/vault/rotations", "rotation_start", `{"id":"${rotation_new}","expected_source_key_id":"${vault_key}","expected_source_key_version":1,"expected_source_key_row_version":1,"target_key_id":"${vault_key_target}","target_key_version":2,"target_algorithm":"AES_256_GCM_RANDOM_NONCE_V1","target_fingerprint":"${vault_fingerprint}"}`, "vault_key", "expected_source_key_id"), 409, "internal/store/vault_key_rotation_mutation.go:92-94 compares the caller's current key with the source key; internal/server/secret.go:652-653 maps conflict to 409.")},
		},
		f2Own("GET /v1/vault/rotations/open", "Open rotation is selected exclusively from the caller's current key owner (vault_key_rotation.go:210).", credAgent, "", false),
		f2Foreign("GET /v1/vault/rotations/{rotation}", "rotation", credAgent, "/v1/vault/rotations/${rotation}", ""),
		f2Foreign("GET /v1/vault/rotations/{rotation}/items", "rotation", credAgent, "/v1/vault/rotations/${rotation}/items", ""),
		{
			Pattern: "POST /v1/vault/rotations/{action}", Class: tenant,
			Handler: "vaultKeyRotationActionHandler", Actions: []string{"stage", "commit", "cancel"}, // vault_key_rotation.go:268-308.
			Probes: []tenantProbe{
				f2FreshProbe("stage", credAgent, "POST", "/v1/vault/rotations/${rotation_stage}:stage", "rotation_stage", `{"expected_rotation_row_version":1,"items":[{"dek_id":"${dek}","expected_source_dek_row_version":1,"expected_source_wrap_revision":1,"target_wrapped_dek":"${vault_wrapped_dek}","target_wrap_revision":2}]}`),
				f2Refusal(f2BodyProbe("stage-dek", credAgent, "POST", "/v1/vault/rotations/${rotation_stage}:stage", "rotation_stage_stage-dek", `{"expected_rotation_row_version":1,"items":[{"dek_id":"${dek}","expected_source_dek_row_version":1,"expected_source_wrap_revision":1,"target_wrapped_dek":"${vault_wrapped_dek}","target_wrap_revision":2}]}`, "dek", "items.0.dek_id"), 409, "internal/store/vault_key_rotation_mutation.go:278-291 scopes the DEK lookup to the caller and returns rotation conflict; internal/server/secret.go:652-653 maps it to 409."),
				f2FreshProbe("commit", credAgent, "POST", "/v1/vault/rotations/${rotation_commit}:commit", "rotation_commit", `{"expected_rotation_row_version":2,"expected_item_count":1,"expected_plan_hash":"${plan_hash}","recovery_disposition":{"mode":"risk_accepted"}}`),
				f2FreshProbe("cancel", credAgent, "POST", "/v1/vault/rotations/${rotation_cancel}:cancel", "rotation_cancel", `{"expected_rotation_row_version":1}`),
			},
		},
		{
			Pattern: "POST /v1/secrets", Class: tenant,
			Probes: []tenantProbe{
				f2SealedCreateProbe(`{"id":"${secret_new}","name":"${secret_name}","description":"${secret_description}","template":"custom","fields":[{"id":"${field_new}","name":"password","kind":"password","sensitive":true,"encoding":"utf8","value_version":1,"sealed":{"envelope_version":1,"ciphertext":"${vault_ciphertext}","algorithm":"AES_256_GCM_RANDOM_NONCE_V1","aad_version":1,"dek":{"id":"${dek_new}","generation":1,"wrapped_dek":"${vault_wrapped_dek}","wrap_algorithm":"AES_256_GCM_RANDOM_NONCE_V1","aad_version":1,"wrap_revision":1,"wrapping_key_id":"${vault_key}","wrapping_key_version":1}}}]}`),
			},
		},
		f2Own("GET /v1/secrets:status", "Capacity derives exclusively from the caller's owner principal (secret.go:329).", credAgent, "", false),
		f2Own("GET /v1/secrets", "Lists only the caller's inventory; query fields contain filters, not another owner selector (secret.go:573-610).", credAgent, "", true),
		f2Foreign("GET /v1/secrets/{secret}", "secret", credAgent, "/v1/secrets/${secret}", ""),
		{
			Pattern: "POST /v1/secrets/{action}", Class: tenant,
			Handler: "secretLifecycleHandler", Actions: []string{"archive", "restore", "delete"}, // secret.go:384-401.
			Probes: []tenantProbe{
				f2FreshProbe("archive", credAgent, "POST", "/v1/secrets/${secret_archive}:archive", "secret_archive", `{"expected_row_version":1}`),
				f2FreshProbe("restore", credAgent, "POST", "/v1/secrets/${secret_restore}:restore", "secret_restore", `{"expected_row_version":2}`),
				f2FreshProbe("delete", credAgent, "POST", "/v1/secrets/${secret_delete}:delete", "secret_delete", `{"expected_row_version":1}`),
			},
		},
		{
			Pattern: "POST /v1/secrets/{secret}/fields/{action}", Class: tenant,
			Handler: "accessSecretFieldHandler", Actions: []string{"access"}, // secret.go:534-546.
			Probes: []tenantProbe{tenantProbeFor("access", foreign, credAgent, "POST", "/v1/secrets/${secret}/fields/${field}:access", `{}`)},
		},

		f2Own("GET /v1/memory-curation-preflight", "Reports the caller's effective credential and capacity; there is no foreign selector (memory_curation.go:260).", credCuratorPreview, "", false),
		{
			Pattern: "POST /v1/memories", Class: tenant,
			Probes: []tenantProbe{
				f2CaptureEvidenceProbe("transcript-evidence", `{"content":"${capture_transcript_content}","kind":"observation","capture_reason":"user_request","evidence":[{"state":"resolved","type":"transcript","transcript_id":"${transcript}","entry_from_sequence":1,"entry_until_sequence":1}]}`),
				f2CaptureEvidenceProbe("memory-evidence", `{"content":"${capture_memory_content}","kind":"observation","capture_reason":"user_request","evidence":[{"state":"resolved","type":"memory","source_memory_id":"${memory}","source_memory_version":2}]}`),
				f2CaptureEvidenceProbe("message-evidence", `{"content":"${capture_message_content}","kind":"observation","capture_reason":"user_request","evidence":[{"state":"resolved","type":"message","message_id":"${message}"}]}`),
			},
		},
		{
			Pattern: "GET /v1/memories", Class: tenant,
			Probes: []tenantProbe{
				{Name: "own", Kind: own, Cred: credAgent, Build: tenantBuild("GET", "/v1/memories", "", ""), ShowsOwn: true},
				{Name: "owner-filter", Kind: foreign, Cred: credAgent, Build: tenantBuild("GET", "/v1/memories", "owner_agent_id=${agent}", ""), Refuse: []int{403}, RefuseWhy: "internal/server/memory.go:1004-1005 maps forbidden owner filters to 403; internal/store/memory.go:2141-2142 rejects both foreign and missing owner ids before lookup."},
			},
		},
		{
			Pattern: "POST /v1/memories:recall", Class: tenant,
			Probes: []tenantProbe{
				{Name: "lexical-own", Kind: own, Cred: credAgent, Build: tenantBuild("POST", "/v1/memories:recall", "", `{"kind":"observation","limit":20}`), ShowsOwn: true},
				f2BodyProbe("vector-profile", credAgent, "POST", "/v1/memories:recall", "", `{"vector_profile_id":"${vector_profile}","query_vector":[1,0],"limit":20}`, "vector_profile", "vector_profile_id"),
			},
		},
		f2Own("GET /v1/memories:status", "Returns only the caller's owner capacity (memory.go:516-536).", credAgent, "", false),
		f2Foreign("GET /v1/memories/{memory}", "memory", credAgent, "/v1/memories/${memory}", ""),
		f2Foreign("GET /v1/memories/{memory}/history", "history", credAgent, "/v1/memories/${memory}/history", ""),
		f2ForeignFresh("PATCH /v1/memories/{memory}", "adjust", credAgent, "/v1/memories/${memory_adjust}", "memory_adjust", `{"expected_version":2,"set_content":"${adjust_content}"}`),
		{
			Pattern: "POST /v1/memories/{memory}/supersede", Class: tenant,
			Probes: []tenantProbe{
				f2FreshProbe("source-and-evidence", credAgent, "POST", "/v1/memories/${memory_supersede}/supersede", "memory_supersede", `{"expected_version":2,"reason":"matrix replacement","replacements":[{"content":"${supersede_content}","kind":"observation","capture_reason":"user_request","idempotency_key":"matrix-replacement","evidence":[{"state":"resolved","type":"transcript","transcript_id":"${transcript}","entry_from_sequence":1,"entry_until_sequence":1}]}]}`),
				f2Refusal(f2BodyProbe("transcript-body", credAgent, "POST", "/v1/memories/${memory_supersede}/supersede", "memory_supersede_transcript-body", `{"expected_version":2,"reason":"matrix replacement","replacements":[{"content":"${supersede_content}","kind":"observation","capture_reason":"user_request","idempotency_key":"matrix-transcript-body-replacement","evidence":[{"state":"resolved","type":"transcript","transcript_id":"${transcript}","entry_from_sequence":1,"entry_until_sequence":1}]}]}`, "transcript", "replacements.0.evidence.0.transcript_id"), 400, "internal/store/memory.go:1686-1747 owner-scopes evidence sources; internal/server/memory.go:1003-1004 maps invalid evidence to 400."),
				f2FreshProbe("memory-evidence", credAgent, "POST", "/v1/memories/${memory_supersede_memory}/supersede", "memory_supersede_memory", `{"expected_version":2,"replacements":[{"content":"${supersede_memory_content}","kind":"observation","capture_reason":"user_request","idempotency_key":"matrix-memory-replacement","evidence":[{"state":"resolved","type":"memory","source_memory_id":"${memory}","source_memory_version":2}]}]}`),
				f2Refusal(f2BodyProbe("memory-body", credAgent, "POST", "/v1/memories/${memory_supersede_memory}/supersede", "memory_supersede_memory_memory-body", `{"expected_version":2,"replacements":[{"content":"${supersede_memory_content}","kind":"observation","capture_reason":"user_request","idempotency_key":"matrix-memory-body-replacement","evidence":[{"state":"resolved","type":"memory","source_memory_id":"${memory}","source_memory_version":2}]}]}`, "memory", "replacements.0.evidence.0.source_memory_id"), 400, "internal/store/memory.go:1686-1747 owner-scopes evidence sources; internal/server/memory.go:1003-1004 maps invalid evidence to 400."),
				f2FreshProbe("message-evidence", credAgent, "POST", "/v1/memories/${memory_supersede_message}/supersede", "memory_supersede_message", `{"expected_version":2,"replacements":[{"content":"${supersede_message_content}","kind":"observation","capture_reason":"user_request","idempotency_key":"matrix-message-replacement","evidence":[{"state":"resolved","type":"message","message_id":"${message}"}]}]}`),
				f2Refusal(f2BodyProbe("message-body", credAgent, "POST", "/v1/memories/${memory_supersede_message}/supersede", "memory_supersede_message_message-body", `{"expected_version":2,"replacements":[{"content":"${supersede_message_content}","kind":"observation","capture_reason":"user_request","idempotency_key":"matrix-message-body-replacement","evidence":[{"state":"resolved","type":"message","message_id":"${message}"}]}]}`, "message", "replacements.0.evidence.0.message_id"), 400, "internal/store/memory.go:1686-1747 owner-scopes evidence sources; internal/server/memory.go:1003-1004 maps invalid evidence to 400."),
			},
		},
		{
			Pattern: "POST /v1/memories/{action}", Class: tenant,
			Handler: "memoryLifecycleHandler", Actions: []string{"forget", "restore", "reactivate"}, // memory.go:747-753.
			Probes: []tenantProbe{
				f2FreshProbe("forget", credAgent, "POST", "/v1/memories/${memory_forget}:forget", "memory_forget", `{"expected_version":2,"reason":"matrix lifecycle"}`),
				f2FreshProbe("restore", credAgent, "POST", "/v1/memories/${memory_restore}:restore", "memory_restore", `{"expected_version":2,"reason":"matrix lifecycle"}`),
				f2FreshProbe("reactivate", credAgent, "POST", "/v1/memories/${memory_reactivate}:reactivate", "memory_reactivate", `{"expected_version":2,"expected_supersession_set_revision":1,"reason":"matrix lifecycle"}`),
			},
		},
		{
			Pattern: "POST /v1/memory-evidence/{evidence}/resolution", Class: tenant,
			Probes: []tenantProbe{
				f2FreshProbe("transcript", credAgent, "POST", "/v1/memory-evidence/${evidence}/resolution", "evidence", `{"transcript_id":"${transcript}","entry_from_sequence":1,"entry_until_sequence":1}`),
				f2Refusal(f2BodyProbe("transcript-body", credAgent, "POST", "/v1/memory-evidence/${evidence}/resolution", "evidence_transcript-body", `{"transcript_id":"${transcript}","entry_from_sequence":1,"entry_until_sequence":1}`, "transcript", "transcript_id"), 400, "internal/store/memory.go:1686-1747 owner-scopes evidence sources; internal/server/memory.go:1003-1004 maps invalid evidence to 400."),
				f2FreshProbe("memory", credAgent, "POST", "/v1/memory-evidence/${evidence_memory}/resolution", "evidence_memory", `{"source_memory_id":"${memory}","source_memory_version":2}`),
				f2Refusal(f2BodyProbe("memory-body", credAgent, "POST", "/v1/memory-evidence/${evidence_memory}/resolution", "evidence_memory_memory-body", `{"source_memory_id":"${memory}","source_memory_version":2}`, "memory", "source_memory_id"), 400, "internal/store/memory.go:1686-1747 owner-scopes evidence sources; internal/server/memory.go:1003-1004 maps invalid evidence to 400."),
				f2FreshProbe("message", credAgent, "POST", "/v1/memory-evidence/${evidence_message}/resolution", "evidence_message", `{"message_id":"${message}"}`),
				f2Refusal(f2BodyProbe("message-body", credAgent, "POST", "/v1/memory-evidence/${evidence_message}/resolution", "evidence_message_message-body", `{"message_id":"${message}"}`, "message", "message_id"), 400, "internal/store/memory.go:1686-1747 owner-scopes evidence sources; internal/server/memory.go:1003-1004 maps invalid evidence to 400."),
			},
		},
		{
			Pattern: "DELETE /v1/memories/{memory}", Class: tenant,
			Probes: []tenantProbe{{Name: "preview", Kind: foreign, Cred: credAgent, Fresh: "memory_delete", NoIdempotencyKey: true, NoIdempotencyKeyWhy: "internal/server/memory.go:842-845 rejects mutation guards on dry_run=true previews.", Build: tenantBuild("DELETE", "/v1/memories/${memory_delete}", "dry_run=true", "")}},
		},
		f2Own("POST /v1/memory-vector-profiles", "Creates an immutable client recipe for the caller; none of its fields select an existing foreign object (memory_vector.go:25-33).", credAgent, `{"provider":"${vector_provider}","model":"${vector_model}","recipe":"${vector_recipe}","recipe_version":"1","dimensions":2,"distance_metric":"cosine","normalization":"none"}`, false),
		f2Own("GET /v1/memory-vector-profiles", "Lists only the caller's profiles (internal/store/memory_vector.go:271).", credAgent, "", true),
		{
			Pattern: "POST /v1/memory-vectors", Class: tenant,
			Probes: []tenantProbe{
				f2FreshProbe("profile-and-memory", credAgent, "POST", "/v1/memory-vectors", "memory_vectors", `{"profile_id":"${vector_profile}","memory_id":"${memory}","memory_version":2,"content_hash":"${content_hash}","vector":[1,0]}`),
				f2BodyProbe("profile-body", credAgent, "POST", "/v1/memory-vectors", "memory_vectors_profile-body", `{"profile_id":"${vector_profile}","memory_id":"${memory}","memory_version":2,"content_hash":"${content_hash}","vector":[1,0]}`, "vector_profile", "profile_id"),
				f2BodyProbe("memory-body", credAgent, "POST", "/v1/memory-vectors", "memory_vectors_memory-body", `{"profile_id":"${vector_profile}","memory_id":"${memory}","memory_version":2,"content_hash":"${content_hash}","vector":[1,0]}`, "memory", "memory_id"),
			},
		},
		// These three full-only curation routes intentionally use full-agent
		// credentials: memory_curation.go:665-688 excludes create/cancel/rollback
		// from BOTH curator profiles. A curator probe could only test 403 before
		// object lookup, and its owner control could never establish isolation.
		f2Own("POST /v1/memory-curation-requests", "Full-agent-only create; scope applies to the caller and names no foreign object (memory_curation.go:60-77,665-688).", credAgent, `{"scope":{"sources":["memory"],"memory_states":["active"],"max_memories":10,"max_evidence":10,"max_transcript_entries":10},"coalescing_key":"${curation_key}","trigger_reason":"manual"}`, false),
		f2Own("GET /v1/memory-curation-requests", "Lists only the caller's curation requests; no foreign owner selector (memory_curation.go:346-381).", credCuratorPreview, "", true),
		f2Foreign("GET /v1/memory-curation-requests/{request}", "request", credCuratorPreview, "/v1/memory-curation-requests/${curation_request}", ""),
		f2ForeignFresh("POST /v1/memory-curation-requests/{request}/start", "start", credCuratorPreview, "/v1/memory-curation-requests/${curation_request_start}/start", "curation_request_start", `{"lease_seconds":300,"client":{"runtime":"tenant-matrix"}}`),
		f2Foreign("GET /v1/memory-curation-runs/{run}", "run", credCuratorPreview, "/v1/memory-curation-runs/${curation_run}", ""),
		{
			Pattern: "GET /v1/memory-curation-runs/{run}/inputs", Class: tenant,
			Probes: []tenantProbe{{Name: "inputs", Kind: foreign, Cred: credCuratorPreview, Build: tenantBuild("GET", "/v1/memory-curation-runs/${curation_run}/inputs", "fencing_generation=1", "")}},
		},
		{
			Pattern: "GET /v1/memory-curation-runs/{run}/plan", Class: tenant,
			Probes: []tenantProbe{{Name: "plan", Kind: foreign, Cred: credCuratorPreview, Build: tenantBuild("GET", "/v1/memory-curation-runs/${curation_run}/plan", "fencing_generation=1", "")}},
		},
		f2ForeignFresh("POST /v1/memory-curation-runs/{run}/renew", "renew", credCuratorPreview, "/v1/memory-curation-runs/${curation_run_renew}/renew", "curation_run_renew", `{"fencing_generation":1,"extension_seconds":300}`),
		{
			Pattern: "POST /v1/memory-curation-runs/{run}/plan", Class: tenant,
			Probes: []tenantProbe{
				f2FreshProbe("plan-and-memory-target", credCuratorPreview, "POST", "/v1/memory-curation-runs/${curation_run_plan}/plan", "curation_run_plan", `{"fencing_generation":1,"draft":{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[{"ordinal":1,"operation":"replace","replace":{"target":{"memory_id":"${memory}","expected_version":2},"snapshot":{"content":"${curation_plan_content}","kind":"observation","evidence":[{"input_evidence_id":"${evidence}","type":"conversation","resolution_state":"resolved","resolved_kind":"transcript","source_transcript_id":"${transcript}","source_sequence_from":1,"source_sequence_until":1}]}}}]}}`),
				f2Refusal(f2BodyProbe("target-memory-body", credCuratorPreview, "POST", "/v1/memory-curation-runs/${curation_run_plan}/plan", "curation_run_plan_target-memory-body", `{"fencing_generation":1,"draft":{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[{"ordinal":1,"operation":"replace","replace":{"target":{"memory_id":"${memory}","expected_version":2},"snapshot":{"content":"${curation_plan_content}","kind":"observation","evidence":[{"input_evidence_id":"${evidence}","type":"conversation","resolution_state":"resolved","resolved_kind":"transcript","source_transcript_id":"${transcript}","source_sequence_from":1,"source_sequence_until":1}]}}}]}}`, "memory", "draft.actions.0.replace.target.memory_id"), 409, "internal/store/memory_curation_plan_store.go:921-923 rejects memory outside run inputs; internal/server/memory_curation.go:811-828 maps curation conflict to 409."),
				f2Refusal(f2BodyProbe("input-evidence-body", credCuratorPreview, "POST", "/v1/memory-curation-runs/${curation_run_plan}/plan", "curation_run_plan_input-evidence-body", `{"fencing_generation":1,"draft":{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[{"ordinal":1,"operation":"replace","replace":{"target":{"memory_id":"${memory}","expected_version":2},"snapshot":{"content":"${curation_plan_content}","kind":"observation","evidence":[{"input_evidence_id":"${evidence}","type":"conversation","resolution_state":"resolved","resolved_kind":"transcript","source_transcript_id":"${transcript}","source_sequence_from":1,"source_sequence_until":1}]}}}]}}`, "evidence", "draft.actions.0.replace.snapshot.evidence.0.input_evidence_id"), 409, "internal/store/memory_curation_plan_store.go:962-968 rejects evidence outside run inputs; internal/server/memory_curation.go:811-828 maps curation conflict to 409."),
				f2Refusal(f2BodyProbe("source-transcript-body", credCuratorPreview, "POST", "/v1/memory-curation-runs/${curation_run_plan}/plan", "curation_run_plan_source-transcript-body", `{"fencing_generation":1,"draft":{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[{"ordinal":1,"operation":"replace","replace":{"target":{"memory_id":"${memory}","expected_version":2},"snapshot":{"content":"${curation_plan_content}","kind":"observation","evidence":[{"input_evidence_id":"${evidence}","type":"conversation","resolution_state":"resolved","resolved_kind":"transcript","source_transcript_id":"${transcript}","source_sequence_from":1,"source_sequence_until":1}]}}}]}}`, "transcript", "draft.actions.0.replace.snapshot.evidence.0.source_transcript_id"), 409, "internal/store/memory_curation_plan_store.go:966-968 rejects mismatched input evidence; internal/server/memory_curation.go:811-828 maps curation conflict to 409."),
			},
		},
		f2ForeignFresh("POST /v1/memory-curation-runs/{run}/apply", "apply", credCuratorApply, "/v1/memory-curation-runs/${curation_run_apply}/apply", "curation_run_apply", `{"fencing_generation":1,"plan_revision":1,"plan_hash":"${plan_hash}"}`),
		{Pattern: "POST /v1/memory-curation-runs/{run}/cancel", Class: tenant, Probes: []tenantProbe{
			f2Refusal(f2FreshProbe("cancel", credAgent, "POST", "/v1/memory-curation-runs/${curation_run_cancel}/cancel", "curation_run_cancel", `{"fencing_generation":1,"reason":"matrix_cancellation"}`), 409, "internal/store/memory_curation.go:559-560,1937-1938 compares a full agent's own lane fence, identically rejecting foreign and never-issued run IDs; internal/server/memory_curation.go:825-826 maps fence mismatch to 409."),
		}},
		f2ForeignFresh("POST /v1/memory-curation-runs/{run}/abandon", "abandon", credCuratorPreview, "/v1/memory-curation-runs/${curation_run_abandon}/abandon", "curation_run_abandon", `{"fencing_generation":1,"reason":"matrix_abandonment"}`),
		{
			Pattern: "POST /v1/memory-curation-runs/{run}/rollback", Class: tenant,
			Probes: []tenantProbe{
				f2FreshProbe("receipt-and-head", credAgent, "POST", "/v1/memory-curation-runs/${curation_run_rollback}/rollback", "curation_run_rollback", `{"apply_receipt_id":"${curation_receipt}","expected_produced_heads":[{"memory_id":"${memory}","version":2}],"reason":"matrix rollback"}`),
				f2Refusal(f2BodyProbe("receipt-body", credAgent, "POST", "/v1/memory-curation-runs/${curation_run_rollback}/rollback", "curation_run_rollback_receipt-body", `{"apply_receipt_id":"${curation_receipt}","expected_produced_heads":[{"memory_id":"${memory}","version":2}],"reason":"matrix rollback"}`, "curation_receipt", "apply_receipt_id"), 409, "internal/store/memory_curation_rollback.go:166-168 rejects a mismatched receipt; internal/server/memory_curation.go:811-828 maps curation conflict to 409."),
				f2Refusal(f2BodyProbe("produced-head-body", credAgent, "POST", "/v1/memory-curation-runs/${curation_run_rollback}/rollback", "curation_run_rollback_produced-head-body", `{"apply_receipt_id":"${curation_receipt}","expected_produced_heads":[{"memory_id":"${memory}","version":2}],"reason":"matrix rollback"}`, "memory", "expected_produced_heads.0.memory_id"), 409, "internal/store/memory_curation_rollback.go:187-189 rejects mismatched produced heads; internal/server/memory_curation.go:811-828 maps curation conflict to 409."),
			},
		},
		{
			Pattern: "GET /v1/memory-curation-status", Class: tenant,
			Probes: []tenantProbe{
				tenantProbeFor("own", own, credCuratorPreview, "GET", "/v1/memory-curation-status", ""),
				{Name: "run-filter", Kind: foreign, Cred: credCuratorPreview, Build: tenantBuild("GET", "/v1/memory-curation-status", "run_id=${curation_run}", "")}, // internal/store/memory_curation.go:2462-2467 owner-scopes the selected run.
			},
		},

		{
			Pattern: "POST /v1/messages", Class: tenant,
			Probes: []tenantProbe{
				f2BodyProbe("recipient", credAgent, "POST", "/v1/messages", "message_send", `{"to":{"kind":"agent","id":"${peer_agent}"},"subject":"${message_subject}","body":"${message_body}"}`, "peer_agent", "to.id"),
				f2BodyProbe("recipient-set", credAgent, "POST", "/v1/messages", "message_send_set", `{"to":{"kind":"agents","ids":["${peer_agent}"]},"body":"${message_set_body}"}`, "peer_agent", "to.ids.0"),
			},
		},
		{
			Pattern: "GET /v1/messages", Class: tenant,
			Probes: []tenantProbe{
				{Name: "own", Kind: own, Cred: credAgent, Build: tenantBuild("GET", "/v1/messages", "", ""), ShowsOwn: true},
				{Name: "from-filter", Kind: foreign, Cred: credAgent, Build: tenantBuild("GET", "/v1/messages", "from=${peer_agent}", ""), EmptyList: true, EmptyListWhy: "internal/store/message.go:1163-1184 scopes the mailbox by account, realm and caller before the sender filter."},
				{Name: "thread-filter", Kind: foreign, Cred: credAgent, Build: tenantBuild("GET", "/v1/messages", "direction=outbox&thread_id=${message-main-to-peer}", ""), EmptyList: true, EmptyListWhy: "internal/store/message.go:1163-1184 scopes the outbox by account, realm and caller before the thread filter."},
			},
		},
		f2Own("POST /v1/messages:listen", "Non-blocking receive list derives its recipient from the principal (message.go:177).", credAgent, `{"wait_seconds":0}`, true),
		{
			Pattern: "GET /v1/messages/{action}", Class: tenant,
			Handler: "peekMessageHandler", Actions: []string{"peek"}, // message.go:278-280.
			Probes: []tenantProbe{tenantProbeFor("peek", foreign, credAgent, "GET", "/v1/messages/${message}:peek", "")},
		},
		{
			Pattern: "POST /v1/messages/{action}", Class: tenant,
			Handler: "messageActionHandler", Actions: []string{"read", "ack", "reply", "claim", "renew", "release", "complete"}, // message.go:333-336.
			Probes: []tenantProbe{
				f2FreshProbe("read", credAgent, "POST", "/v1/messages/${message_read}:read", "message_read", `{}`),
				f2FreshProbe("ack", credAgent, "POST", "/v1/messages/${message_ack}:ack", "message_ack", `{}`),
				f2FreshProbe("reply", credAgent, "POST", "/v1/messages/${message_reply}:reply", "message_reply", `{"body":"${message_reply_body}"}`),
				f2FreshProbe("claim", credAgent, "POST", "/v1/messages/${message_claim}:claim", "message_claim", `{"lease_seconds":300}`),
				f2FreshProbe("renew", credAgent, "POST", "/v1/messages/${message_renew}:renew", "message_renew", `{"claim_id":"${message_claim_id}","generation":1,"lease_seconds":300}`),
				f2Refusal(f2BodyProbe("renew-claim-body", credAgent, "POST", "/v1/messages/${message_renew}:renew", "message_renew_claim-body", `{"claim_id":"${message_claim_id}","generation":1,"lease_seconds":300}`, "message_claim_id", "claim_id"), 409, "internal/store/message.go:640-642,704-706,802-824 compares the caller-owned message claim fence; internal/server/message.go:537-538 maps stale claims to 409."),
				f2FreshProbe("release", credAgent, "POST", "/v1/messages/${message_release}:release", "message_release", `{"claim_id":"${message_claim_id}","generation":1}`),
				f2Refusal(f2BodyProbe("release-claim-body", credAgent, "POST", "/v1/messages/${message_release}:release", "message_release_claim-body", `{"claim_id":"${message_claim_id}","generation":1}`, "message_claim_id", "claim_id"), 409, "internal/store/message.go:640-642,704-706,802-824 compares the caller-owned message claim fence; internal/server/message.go:537-538 maps stale claims to 409."),
				f2FreshProbe("complete", credAgent, "POST", "/v1/messages/${message_complete}:complete", "message_complete", `{"claim_id":"${message_claim_id}","generation":1,"body":"${message_complete_body}"}`),
				f2Refusal(f2BodyProbe("complete-claim-body", credAgent, "POST", "/v1/messages/${message_complete}:complete", "message_complete_claim-body", `{"claim_id":"${message_claim_id}","generation":1,"body":"${message_complete_body}"}`, "message_claim_id", "claim_id"), 409, "internal/store/message.go:640-642,704-706,802-824 compares the caller-owned message claim fence; internal/server/message.go:537-538 maps stale claims to 409."),
			},
		},
		f2Own("GET /v1/email/address", "Address is selected by the authenticated agent; no request owner selector (agent_email.go:872).", credAgent, "", true),
		f2Own("GET /v1/email:status", "Reports the authenticated mailbox's capacity (agent_email.go:755).", credAgent, "", false),
		f2Own("GET /v1/email", "Inbound inventory is selected by the authenticated mailbox owner (agent_email.go:1031).", credAgent, "", true),
		f2Own("POST /v1/email:listen", "Non-blocking receive list is selected by the authenticated mailbox owner (agent_email.go:1107).", credAgent, `{"wait_seconds":0}`, true),
		f2Own("GET /v1/email/checkpoint", "Checkpoint is selected by the authenticated mailbox owner (agent_email.go:1177).", credAgent, "", false),
		f2Canary("POST /v1/email/retry-canary:arm"),
		f2Canary("POST /v1/email/retry-canary:status"),
		f2Own("POST /v1/email:send", "Creates caller-owned outbound mail; the external recipient address is not a cell object reference (agent_email_outbound.go:89-93).", credAgent, `{"to":"${email_to}","subject":"${email_subject}","text":"${email_body}"}`, false),
		{
			Pattern: "POST /v1/email/{action}", Class: tenant,
			Handler: "agentEmailActionDispatchHandler", Actions: []string{"read", "ack", "code-consumed", "claim", "renew", "release", "complete", "reply"}, // agent_email.go:1285; agent_email_outbound.go dispatches reply.
			Probes: []tenantProbe{
				f2FreshProbe("read", credAgent, "POST", "/v1/email/${inbound_email_read}:read", "inbound_email_read", `{}`),
				f2FreshProbe("ack", credAgent, "POST", "/v1/email/${inbound_email_ack}:ack", "inbound_email_ack", `{}`),
				f2FreshProbe("code-consumed", credAgent, "POST", "/v1/email/${inbound_email_code_consumed}:code-consumed", "inbound_email_code_consumed", `{}`),
				f2FreshProbe("claim", credAgent, "POST", "/v1/email/${inbound_email_claim}:claim", "inbound_email_claim", `{"lease_seconds":300}`),
				f2FreshProbe("renew", credAgent, "POST", "/v1/email/${inbound_email_renew}:renew", "inbound_email_renew", `{"claim_id":"${email_claim}","generation":1,"lease_seconds":300}`),
				f2Refusal(f2BodyProbe("renew-claim-body", credAgent, "POST", "/v1/email/${inbound_email_renew}:renew", "inbound_email_renew_claim-body", `{"claim_id":"${email_claim}","generation":1,"lease_seconds":300}`, "email_claim", "claim_id"), 409, "internal/store/agent_email.go:2070-2072,2118-2120,2187-2189 compares the caller-owned email claim fence; internal/server/agent_email.go:1418-1419 maps conflict to 409."),
				f2FreshProbe("release", credAgent, "POST", "/v1/email/${inbound_email_release}:release", "inbound_email_release", `{"claim_id":"${email_claim}","generation":1,"deterministic_failure":false}`),
				f2Refusal(f2BodyProbe("release-claim-body", credAgent, "POST", "/v1/email/${inbound_email_release}:release", "inbound_email_release_claim-body", `{"claim_id":"${email_claim}","generation":1,"deterministic_failure":false}`, "email_claim", "claim_id"), 409, "internal/store/agent_email.go:2070-2072,2118-2120,2187-2189 compares the caller-owned email claim fence; internal/server/agent_email.go:1418-1419 maps conflict to 409."),
				f2FreshProbe("complete", credAgent, "POST", "/v1/email/${inbound_email_complete}:complete", "inbound_email_complete", `{"claim_id":"${email_claim}","generation":1}`),
				f2Refusal(f2BodyProbe("complete-claim-body", credAgent, "POST", "/v1/email/${inbound_email_complete}:complete", "inbound_email_complete_claim-body", `{"claim_id":"${email_claim}","generation":1}`, "email_claim", "claim_id"), 409, "internal/store/agent_email.go:2070-2072,2118-2120,2187-2189 compares the caller-owned email claim fence; internal/server/agent_email.go:1418-1419 maps conflict to 409."),
				f2FreshProbe("reply", credAgent, "POST", "/v1/email/${inbound_email_reply}:reply", "inbound_email_reply", `{"text":"${email_reply_body}"}`),
			},
		},
		f2Own("GET /v1/email/sent", "Outbox inventory is selected by the authenticated sender (agent_email_outbound.go:listAgentEmailOutboundHandler).", credAgent, "", true),
		f2Foreign("GET /v1/email/sent/{id}", "outbound-message", credAgent, "/v1/email/sent/${outbound_email}", ""),
		f2Own("POST /v1/message-requests", "Creates a realm-local request with server-derived recipients; identity/routing body fields are rejected (message_request.go:34-36).", credAgent, `{"subject":"${request_subject}","body":"${request_body}","selection_policy":"client_ranked","max_assignees":1,"offer_window_seconds":60}`, false),
		f2Own("GET /v1/message-requests", "Lists requests visible to the authenticated agent in its realm (message_request.go:66).", credAgent, "", true),
		f2Foreign("GET /v1/message-requests/{request}", "request", credAgent, "/v1/message-requests/${message_request}", ""),
		{
			Pattern: "POST /v1/message-requests/{action}", Class: tenant,
			Handler: "messageRequestActionHandler", Actions: []string{"offer", "decline", "select", "cancel", "claim", "renew", "release", "complete"}, // message_request.go:139-153.
			Probes: []tenantProbe{
				f2FreshProbe("offer", credAgent, "POST", "/v1/message-requests/${message_request_offer}:offer", "message_request_offer", `{"body":"${request_offer_body}"}`),
				f2FreshProbe("decline", credAgent, "POST", "/v1/message-requests/${message_request_decline}:decline", "message_request_decline", `{}`),
				f2FreshProbe("select", credAgent, "POST", "/v1/message-requests/${message_request_select}:select", "message_request_select", `{"selected_agent_ids":["${peer_agent}"],"reservation_seconds":300}`),
				f2Refusal(f2BodyProbe("selected-agent-body", credAgent, "POST", "/v1/message-requests/${message_request_select}:select", "message_request_select_selected-agent-body", `{"selected_agent_ids":["${peer_agent}"],"reservation_seconds":300}`, "peer_agent", "selected_agent_ids.0"), 409, "internal/store/message_request.go:1873-1883 rejects agents outside the request offers; internal/server/message_request.go:472-473 maps conflict to 409."),
				f2FreshProbe("cancel", credAgent, "POST", "/v1/message-requests/${message_request_cancel}:cancel", "message_request_cancel", `{}`),
				f2FreshProbe("claim", credAgent, "POST", "/v1/message-requests/${message_request_claim}:claim", "message_request_claim", `{"lease_seconds":300}`),
				f2FreshProbe("renew", credAgent, "POST", "/v1/message-requests/${message_request_renew}:renew", "message_request_renew", `{"claim_id":"${message_request_claim_id}","generation":1,"lease_seconds":300}`),
				f2BodyProbe("renew-claim-body", credAgent, "POST", "/v1/message-requests/${message_request_renew}:renew", "message_request_renew_claim-body", `{"claim_id":"${message_request_claim_id}","generation":1,"lease_seconds":300}`, "message_request_claim_id", "claim_id"),
				f2FreshProbe("release", credAgent, "POST", "/v1/message-requests/${message_request_release}:release", "message_request_release", `{"claim_id":"${message_request_claim_id}","generation":1,"deterministic_failure":false}`),
				f2BodyProbe("release-claim-body", credAgent, "POST", "/v1/message-requests/${message_request_release}:release", "message_request_release_claim-body", `{"claim_id":"${message_request_claim_id}","generation":1,"deterministic_failure":false}`, "message_request_claim_id", "claim_id"),
				f2FreshProbe("complete", credAgent, "POST", "/v1/message-requests/${message_request_complete}:complete", "message_request_complete", `{"claim_id":"${message_request_claim_id}","generation":1,"body":"${request_complete_body}"}`),
				f2BodyProbe("complete-claim-body", credAgent, "POST", "/v1/message-requests/${message_request_complete}:complete", "message_request_complete_claim-body", `{"claim_id":"${message_request_claim_id}","generation":1,"body":"${request_complete_body}"}`, "message_request_claim_id", "claim_id"),
			},
		},
	}
}

func f2Foreign(pattern, name string, cred tenantCred, path, body string) tenantRow {
	method, _, _ := strings.Cut(pattern, " ")
	return tenantRow{Pattern: pattern, Class: tenant, Probes: []tenantProbe{tenantProbeFor(name, foreign, cred, method, path, body)}}
}

func f2ForeignFresh(pattern, name string, cred tenantCred, path, fresh, body string) tenantRow {
	row := f2Foreign(pattern, name, cred, path, body)
	row.Probes[0].Fresh = fresh
	return row
}

func f2FreshProbe(name string, cred tenantCred, method, path, fresh, body string) tenantProbe {
	probe := tenantProbeFor(name, foreign, cred, method, path, body)
	probe.Fresh = fresh
	return probe
}

func f2Own(pattern, reason string, cred tenantCred, body string, showsOwn bool) tenantRow {
	method, path, _ := strings.Cut(pattern, " ")
	probe := tenantProbeFor("own", own, cred, method, path, body)
	probe.ShowsOwn = showsOwn
	return tenantRow{Pattern: pattern, Class: tenant, NoForeignWhy: reason, Probes: []tenantProbe{probe}}
}

func f2OwnFresh(pattern, reason string, cred tenantCred, fresh, body string) tenantRow {
	row := f2Own(pattern, reason, cred, body, false)
	row.Probes[0].Fresh = fresh
	return row
}

func f2Canary(pattern string) tenantRow {
	method, path, _ := strings.Cut(pattern, " ")
	return tenantRow{Pattern: pattern, Class: tenant, Probes: []tenantProbe{{
		Name: "configured-owner", Kind: foreign, Cred: credAgent,
		Build:  tenantBuild(method, path, "", `{"challenge":"${canary_challenge}"}`),
		Refuse: []int{403}, RefuseWhy: "internal/server/agent_email.go:1206-1207 refuses any agent other than the configured retry canary.",
		ConfigBound: "The cell configuration names B main as the retry canary; the request cannot select a different owner.",
	}}}
}

// Foreign evidence ids fail owner-scope validation identically to never-issued
// ids; the source lookup is an EXISTS query and intentionally returns 400.
func f2CaptureEvidenceProbe(name, body string) tenantProbe {
	refs := map[string][2]string{
		"transcript-evidence": {"transcript", "evidence.0.transcript_id"},
		"memory-evidence":     {"memory", "evidence.0.source_memory_id"},
		"message-evidence":    {"message", "evidence.0.message_id"},
	}
	ref := refs[name]
	probe := f2BodyProbe(name, credAgent, "POST", "/v1/memories", "capture_"+name, body, ref[0], ref[1])
	probe.Refuse = []int{400}
	probe.RefuseWhy = "internal/server/memory.go:1003-1004 maps bad input to 400; internal/store/memory.go:1746-1747 rejects absent and foreign evidence sources identically."
	return probe
}

func f2SealedCreateProbe(body string) tenantProbe {
	probe := f2BodyProbe("wrapping-key", credAgent, "POST", "/v1/secrets", "secret_create", body, "vault_key", "fields.0.sealed.dek.wrapping_key_id")
	probe.Refuse = []int{409}
	probe.RefuseWhy = "internal/server/secret.go:648-649 maps vault-key mismatch to 409; internal/store/secret_mutation.go:207-209 compares the caller's key to either a foreign or never-issued wrapping key."
	return probe
}

// f2BodyProbe retains one side-dependent body reference while every prerequisite,
// payload canary and path object belongs to A. T4 verifies the selected B value
// occurs only at BodyRef, independently of this template transformation.
func f2BodyProbe(name string, cred tenantCred, method, path, fresh, body, refKey, bodyRef string) tenantProbe {
	ownTemplate := func(template, selected string) string {
		return tenantPlaceholder.ReplaceAllStringFunc(template, func(token string) string {
			key := token[2 : len(token)-1]
			if key == selected {
				return token
			}
			return "${own." + key + "}"
		})
	}
	return tenantProbe{
		Name: name, Kind: foreign, Cred: cred, Fresh: fresh, BodyRef: bodyRef,
		Build: tenantBuild(method, ownTemplate(path, ""), "", ownTemplate(body, refKey)),
	}
}

func f2Refusal(probe tenantProbe, status int, why string) tenantProbe {
	probe.Refuse = []int{status}
	probe.RefuseWhy = why
	return probe
}
