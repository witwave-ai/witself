package server

// configuredCapabilities reports whether a deployment exposes a route in each
// feature family. Partial callback wiring still exposes its registered routes;
// a family flag does not promise that every operation in the family is wired.
// Authentication and account entitlements remain independent of discovery.
func configuredCapabilities(cfg Config) map[string]bool {
	principal := cfg.AuthenticatePrincipal != nil
	operator := cfg.Authenticate != nil
	transcripts := principal && (cfg.CreateTranscript != nil || cfg.AppendTranscriptEntry != nil ||
		cfg.AppendTranscriptEntries != nil || cfg.ListTranscripts != nil ||
		cfg.GetTranscript != nil || cfg.GetTranscriptPage != nil)
	processing := principal && (cfg.ClaimMessage != nil || cfg.RenewMessageClaim != nil ||
		cfg.ReleaseMessageClaim != nil || cfg.CompleteMessage != nil)
	recall := principal && cfg.RecallMemories != nil
	vectors := principal && (cfg.CreateMemoryVectorProfile != nil ||
		cfg.ListMemoryVectorProfiles != nil || cfg.PutMemoryVector != nil)
	return map[string]bool{
		"facts": principal && (cfg.SetFact != nil || cfg.GetFactLimitStatus != nil ||
			cfg.DeleteFact != nil || (cfg.GetFact != nil && cfg.ListFacts != nil) ||
			cfg.GetFactHistory != nil || cfg.UpcomingFacts != nil || cfg.ProposeFact != nil ||
			cfg.ListFactCandidates != nil || cfg.GetFactCandidate != nil ||
			(cfg.ConfirmFactCandidate != nil && cfg.RejectFactCandidate != nil) ||
			cfg.UpsertFactSubject != nil || cfg.AddFactSubjectAlias != nil || cfg.ListFactSubjects != nil),
		"self_digest": principal,
		"memories": principal && (cfg.CaptureMemory != nil || cfg.ListMemories != nil ||
			cfg.GetMemory != nil || cfg.GetMemoryLimitStatus != nil || cfg.GetMemoryHistory != nil ||
			cfg.AdjustMemory != nil || cfg.SupersedeMemory != nil || cfg.ForgetMemory != nil ||
			cfg.RestoreMemory != nil || cfg.ReactivateMemory != nil || cfg.ResolveMemoryEvidence != nil ||
			cfg.DeleteMemory != nil || cfg.RecallMemories != nil),
		"memory_recall":           recall,
		"memory_supersede":        principal && cfg.SupersedeMemory != nil,
		"memory_permanent_delete": principal && cfg.DeleteMemory != nil,
		"memory_vector_profiles":  vectors,
		"client_vector_recall":    vectors && recall,
		"semantic_recall":         vectors && recall,
		"opportunistic_curation": principal && (cfg.RequestMemoryCuration != nil ||
			cfg.ListMemoryCurationRequests != nil || cfg.GetMemoryCurationRequest != nil ||
			cfg.StartMemoryCuration != nil || cfg.GetMemoryCurationRun != nil ||
			cfg.GetMemoryCurationRunInputs != nil || cfg.GetMemoryCurationPlan != nil ||
			cfg.RenewMemoryCuration != nil || cfg.PlanMemoryCuration != nil ||
			cfg.ApplyMemoryCuration != nil || cfg.CancelMemoryCuration != nil ||
			cfg.AbandonMemoryCuration != nil || cfg.RollbackMemoryCuration != nil ||
			cfg.GetMemoryCurationStatus != nil),
		"transcripts":        transcripts,
		"transcript_capture": transcripts,
		"messaging": principal && (cfg.SendMessage != nil || cfg.ListMessages != nil ||
			cfg.ReadMessage != nil || cfg.AckMessage != nil || cfg.ReplyMessage != nil || processing),
		"message_listen":     principal && cfg.ListMessages != nil,
		"message_reply":      principal && cfg.ReplyMessage != nil,
		"message_processing": processing,
		"message_requests": principal && (cfg.CreateMessageRequest != nil || cfg.ListMessageRequests != nil ||
			cfg.GetMessageRequest != nil || cfg.OfferMessageRequest != nil || cfg.DeclineMessageRequest != nil ||
			cfg.SelectMessageRequest != nil || cfg.CancelMessageRequest != nil || cfg.ClaimMessageRequest != nil ||
			cfg.RenewMessageRequest != nil || cfg.ReleaseMessageRequest != nil || cfg.CompleteMessageRequest != nil),
		"avatars": (principal && (cfg.GetSelfAvatar != nil || cfg.GetSelfAvatarHistory != nil ||
			cfg.GetSelfAvatarVersion != nil || cfg.GetSelfAvatarStyle != nil || cfg.ProposeSelfAvatar != nil ||
			cfg.ActivateSelfAvatar != nil || cfg.RollbackSelfAvatar != nil || cfg.ResetSelfAvatar != nil ||
			cfg.ReportSelfAvatarGenerationFailure != nil)) ||
			(operator && (cfg.GetAgentAvatar != nil || cfg.GetAgentAvatarHistory != nil ||
				cfg.GetAgentAvatarVersion != nil || cfg.ProposeAgentAvatar != nil || cfg.ActivateAgentAvatar != nil ||
				cfg.RejectAgentAvatar != nil || cfg.RollbackAgentAvatar != nil || cfg.ResetAgentAvatar != nil ||
				cfg.UpdateAgentAvatarPolicy != nil || cfg.UpdateAgentAvatarQuota != nil ||
				cfg.GetRealmAvatarStyle != nil || cfg.CreateRealmAvatarStyleVersion != nil)),
		"secrets": principal && (cfg.GetCurrentVaultKey != nil || cfg.RegisterVaultKey != nil ||
			cfg.CreateVaultKeyEnrollment != nil || cfg.ListVaultKeyEnrollments != nil || cfg.GetVaultKeyEnrollment != nil ||
			cfg.ApproveVaultKeyEnrollment != nil || cfg.ReceiveVaultKeyEnrollment != nil ||
			cfg.ConsumeVaultKeyEnrollment != nil || cfg.CancelVaultKeyEnrollment != nil ||
			cfg.StartVaultKeyRotation != nil || cfg.GetOpenVaultKeyRotation != nil || cfg.GetVaultKeyRotation != nil ||
			cfg.ListVaultKeyRotationItems != nil || cfg.StageVaultKeyRotation != nil || cfg.CommitVaultKeyRotation != nil ||
			cfg.CancelVaultKeyRotation != nil || cfg.CreateSecret != nil || cfg.GetSecretLimitStatus != nil ||
			cfg.ListSecrets != nil || cfg.GetSecret != nil || cfg.ArchiveSecret != nil || cfg.RestoreSecret != nil ||
			cfg.DeleteSecret != nil || cfg.AccessSecretField != nil),
		"agent_email_send":  principal && cfg.QueueAgentEmail != nil,
		"agent_email_reply": principal && cfg.ReplyAgentEmail != nil,
		"agent_email_sent_history": principal && (cfg.ListAgentEmailOutbox != nil ||
			cfg.GetAgentEmailOutbound != nil),
		"audit": (operator && cfg.ListAccountEvents != nil) ||
			(cfg.ProvisionToken != "" && cfg.ListAdminEventsAll != nil),
		"automatic_capture":  false,
		"scheduled_curation": false,
		"policies":           false,
		"groups":             false,
	}
}
