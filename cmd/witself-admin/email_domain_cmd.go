package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/id"
)

func emailDomainAdminCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr,
			"usage: witself-admin email-domain (requests|audit|journal|recovery) ...")
		return 2
	}
	switch args[0] {
	case "requests":
		return emailDomainAdminRequests(args[1:])
	case "audit":
		return emailDomainAdminAudit(args[1:])
	case "journal":
		return emailDomainAdminJournal(args[1:])
	case "recovery":
		return emailDomainAdminRecovery(args[1:])
	default:
		fmt.Fprintf(os.Stderr,
			"witself-admin email-domain: unknown subcommand %q\n", args[0])
		return 2
	}
}

type emailDomainRecoveryCommon struct {
	emailDomainAdminCommon
	recoveryTokenFile *string
}

func newEmailDomainRecoveryCommon(name string, mutation bool) emailDomainRecoveryCommon {
	common := newEmailDomainAdminCommon(name, mutation)
	if mutation {
		common.flagSet.Lookup("idempotency-key").Usage =
			"required exact retry key"
	}
	return emailDomainRecoveryCommon{
		emailDomainAdminCommon: common,
		recoveryTokenFile: common.flagSet.String("recovery-token-file", "",
			"file containing the distinct custom-domain recovery token"),
	}
}

func resolveAgentEmailDomainRecoveryToken(flagValue string) (string, error) {
	if path := strings.TrimSpace(flagValue); path != "" {
		return readAgentEmailDomainRecoveryTokenFile(path)
	}
	if path := strings.TrimSpace(os.Getenv(
		"WITSELF_AGENT_EMAIL_DOMAIN_RECOVERY_TOKEN_FILE")); path != "" {
		return readAgentEmailDomainRecoveryTokenFile(path)
	}
	if path, err := managedTokenPath("agent-email-domain-recovery.token"); err == nil {
		if token, readErr := readAgentEmailDomainRecoveryTokenFile(path); readErr == nil {
			return token, nil
		}
	}
	return "", fmt.Errorf("no custom-domain recovery token file — use --recovery-token-file, WITSELF_AGENT_EMAIL_DOMAIN_RECOVERY_TOKEN_FILE, or ~/.witself/tokens/agent-email-domain-recovery.token")
}

func readAgentEmailDomainRecoveryTokenFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect custom-domain recovery token file %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("custom-domain recovery token file %q must be a regular file, not a symlink", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("custom-domain recovery token file %q must be owner-only (mode 0600 or stricter)", path)
	}
	return readTokenFile(path)
}

func (c emailDomainRecoveryCommon) recoveryCredentials() (string, string, string, error) {
	endpoint, adminToken, err := c.credentials()
	if err != nil {
		return "", "", "", err
	}
	recoveryToken, err := resolveAgentEmailDomainRecoveryToken(*c.recoveryTokenFile)
	if err != nil {
		return "", "", "", err
	}
	return endpoint, adminToken, recoveryToken, nil
}

func (c emailDomainRecoveryCommon) exactMutation() (string, string, error) {
	reason := strings.TrimSpace(*c.reason)
	key := strings.TrimSpace(*c.idempotency)
	if reason == "" {
		return "", "", fmt.Errorf("--reason is required")
	}
	if key == "" {
		return "", "", fmt.Errorf("--idempotency-key is required for retry-safe journal/recovery work")
	}
	return key, reason, nil
}

type emailDomainAdminCommon struct {
	endpoint    *string
	token       *string
	tokenFile   *string
	idempotency *string
	reason      *string
	json        *bool
	flagSet     *flag.FlagSet
}

func newEmailDomainAdminCommon(name string, mutation bool) emailDomainAdminCommon {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	common := emailDomainAdminCommon{
		flagSet:   fs,
		endpoint:  fs.String("endpoint", "", "control-plane URL"),
		token:     fs.String("token", "", "admin token"),
		tokenFile: fs.String("token-file", "", "file containing the admin token"),
		json:      jsonFlag(fs),
	}
	if mutation {
		common.idempotency = fs.String("idempotency-key", "",
			"retry key (generated when omitted)")
		common.reason = fs.String("reason", "", "required audit reason")
	}
	return common
}

func (c emailDomainAdminCommon) credentials() (string, string, error) {
	token, err := resolveAdminToken(*c.token, *c.tokenFile)
	if err != nil {
		return "", "", err
	}
	return cpEndpoint(*c.endpoint), token, nil
}

func (c emailDomainAdminCommon) mutation() (string, string, error) {
	reason := strings.TrimSpace(*c.reason)
	if reason == "" {
		return "", "", fmt.Errorf("--reason is required")
	}
	key := strings.TrimSpace(*c.idempotency)
	if key == "" {
		var err error
		key, err = id.New("admin_email_domain")
		if err != nil {
			return "", "", fmt.Errorf("generate idempotency key: %w", err)
		}
	}
	return key, reason, nil
}

func emailDomainAdminRequests(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr,
			"usage: witself-admin email-domain requests (list|show|verify|reject|retire) ...")
		return 2
	}
	action := args[0]
	switch action {
	case "list":
		common := newEmailDomainAdminCommon("email-domain requests list", false)
		state := common.flagSet.String("state", "", "filter by request state")
		accountID := common.flagSet.String("account", "", "filter by account id")
		domain := common.flagSet.String("domain", "", "filter by exact domain")
		cursor := common.flagSet.String("cursor", "", "continue from an opaque next-page cursor")
		if err := common.flagSet.Parse(args[1:]); err != nil || common.flagSet.NArg() != 0 {
			return 2
		}
		endpoint, token, err := common.credentials()
		if err != nil {
			return printEmailDomainAdminError(err, 2)
		}
		page, err := client.ListAdminAgentEmailDomainRequestsPage(
			context.Background(), endpoint, token,
			client.AdminAgentEmailDomainRequestFilter{
				State: *state, AccountID: *accountID, Domain: *domain, Cursor: *cursor,
			})
		if err != nil {
			return printEmailDomainAdminError(err, 1)
		}
		if page.Requests == nil {
			page.Requests = []client.AgentEmailDomainRequest{}
		}
		if *common.json {
			return printJSON(page)
		}
		w, flush := tableWriter(
			"id\taccount\tdomain\tstate\tavailability\tverification\tlast_result\tupdated")
		for i := range page.Requests {
			request := &page.Requests[i]
			verification, lastResult := emailDomainAdminVerificationColumns(request)
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				emailDomainAdminColumn(request.RequestID),
				emailDomainAdminColumn(request.AccountID),
				emailDomainAdminColumn(request.Domain),
				emailDomainAdminColumn(request.State),
				emailDomainAdminColumn(request.Availability),
				emailDomainAdminColumn(verification),
				emailDomainAdminColumn(lastResult),
				emailDomainAdminOptionalTime(request.UpdatedAt))
		}
		flush()
		printEmailDomainAdminNextCursor(page.NextCursor)
		return 0

	case "show":
		common := newEmailDomainAdminCommon("email-domain requests show", false)
		requestID := common.flagSet.String("request", "", "request id (required)")
		if err := common.flagSet.Parse(args[1:]); err != nil ||
			common.flagSet.NArg() != 0 || strings.TrimSpace(*requestID) == "" {
			fmt.Fprintln(os.Stderr,
				"usage: witself-admin email-domain requests show --request REQUEST_ID")
			return 2
		}
		endpoint, token, err := common.credentials()
		if err != nil {
			return printEmailDomainAdminError(err, 2)
		}
		request, err := client.GetAdminAgentEmailDomainRequest(
			context.Background(), endpoint, token, *requestID)
		if err != nil {
			return printEmailDomainAdminError(err, 1)
		}
		return printEmailDomainAdminRequest(request, *common.json, true)

	case "verify":
		common := newEmailDomainAdminCommon("email-domain requests verify", false)
		requestID := common.flagSet.String("request", "", "request id (required)")
		idempotencyKey := common.flagSet.String("idempotency-key", "",
			"required exact retry key")
		if err := common.flagSet.Parse(args[1:]); err != nil ||
			common.flagSet.NArg() != 0 || strings.TrimSpace(*requestID) == "" ||
			strings.TrimSpace(*idempotencyKey) == "" {
			fmt.Fprintln(os.Stderr,
				"usage: witself-admin email-domain requests verify --request REQUEST_ID --idempotency-key KEY")
			return 2
		}
		key := strings.TrimSpace(*idempotencyKey)
		endpoint, token, err := common.credentials()
		if err != nil {
			return printEmailDomainAdminError(err, 2)
		}
		request, err := client.VerifyAdminAgentEmailDomainRequest(
			context.Background(), endpoint, token, *requestID, key)
		if err != nil {
			return printEmailDomainAdminError(err, 1)
		}
		return printEmailDomainAdminRequest(request, *common.json, true)

	case "reject", "retire":
		common := newEmailDomainAdminCommon("email-domain requests "+action, true)
		requestID := common.flagSet.String("request", "", "request id (required)")
		if err := common.flagSet.Parse(args[1:]); err != nil ||
			common.flagSet.NArg() != 0 || strings.TrimSpace(*requestID) == "" {
			fmt.Fprintf(os.Stderr,
				"usage: witself-admin email-domain requests %s --request REQUEST_ID --reason REASON\n",
				action)
			return 2
		}
		key, reason, err := common.mutation()
		if err != nil {
			return printEmailDomainAdminError(err, 2)
		}
		endpoint, token, err := common.credentials()
		if err != nil {
			return printEmailDomainAdminError(err, 2)
		}
		var request *client.AgentEmailDomainRequest
		if action == "reject" {
			request, err = client.RejectAdminAgentEmailDomainRequest(
				context.Background(), endpoint, token, *requestID, key, reason)
		} else {
			request, err = client.RetireAdminAgentEmailDomainRequest(
				context.Background(), endpoint, token, *requestID, key, reason)
		}
		if err != nil {
			return printEmailDomainAdminError(err, 1)
		}
		return printEmailDomainAdminRequest(request, *common.json, false)

	default:
		fmt.Fprintf(os.Stderr,
			"witself-admin email-domain requests: unknown action %q\n", action)
		return 2
	}
}

func emailDomainAdminJournal(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr,
			"usage: witself-admin email-domain journal (status|bootstrap|checkpoint) ...")
		return 2
	}
	action := args[0]
	if action != "status" && action != "bootstrap" && action != "checkpoint" {
		fmt.Fprintf(os.Stderr,
			"witself-admin email-domain journal: unknown action %q\n", action)
		return 2
	}
	common := newEmailDomainRecoveryCommon("email-domain journal "+action,
		action != "status")
	if err := common.flagSet.Parse(args[1:]); err != nil || common.flagSet.NArg() != 0 {
		return 2
	}
	endpoint, adminToken, recoveryToken, err := common.recoveryCredentials()
	if err != nil {
		return printEmailDomainAdminError(err, 2)
	}
	if action == "status" {
		status, getErr := client.GetAdminAgentEmailDomainJournal(
			context.Background(), endpoint, adminToken, recoveryToken)
		if getErr != nil {
			return printEmailDomainAdminError(getErr, 1)
		}
		if *common.json {
			return printJSON(status)
		}
		printEmailDomainJournalStatus(status)
		return 0
	}
	key, reason, err := common.exactMutation()
	if err != nil {
		return printEmailDomainAdminError(err, 2)
	}
	var progress *client.AgentEmailDomainJournalProgress
	if action == "bootstrap" {
		progress, err = client.BootstrapAdminAgentEmailDomainJournal(
			context.Background(), endpoint, adminToken, recoveryToken, reason, key)
	} else {
		progress, err = client.CheckpointAdminAgentEmailDomainJournal(
			context.Background(), endpoint, adminToken, recoveryToken, reason, key)
	}
	if err != nil {
		return printEmailDomainAdminError(err, 1)
	}
	if *common.json {
		return printJSON(progress)
	}
	printEmailDomainJournalProgress(progress)
	return 0
}

func emailDomainAdminRecovery(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr,
			"usage: witself-admin email-domain recovery (start|status|advance|verify) ...")
		return 2
	}
	action := args[0]
	if action != "start" && action != "status" && action != "advance" &&
		action != "verify" {
		fmt.Fprintf(os.Stderr,
			"witself-admin email-domain recovery: unknown action %q\n", action)
		return 2
	}
	common := newEmailDomainRecoveryCommon("email-domain recovery "+action,
		action == "start")
	recoveryID := common.flagSet.String("recovery", "", "aedrec_ recovery id (required)")
	sourceStream := common.flagSet.String("source-stream", "", "exact aedj_ source stream (start only)")
	expectedSequence := common.flagSet.Int64("expected-sequence", 0, "exact journal-head sequence (start only)")
	expectedHash := common.flagSet.String("expected-hash", "", "exact journal-head SHA-256 (start only)")
	expectedActionFence := common.flagSet.String("expected-action-fence", "", "exact current action fence (advance/verify only)")
	actionKey := common.idempotency
	if action != "start" {
		actionKey = common.flagSet.String("idempotency-key", "",
			"exact retry key (advance/verify only)")
	}
	if err := common.flagSet.Parse(args[1:]); err != nil || common.flagSet.NArg() != 0 ||
		strings.TrimSpace(*recoveryID) == "" {
		return 2
	}
	endpoint, adminToken, recoveryToken, err := common.recoveryCredentials()
	if err != nil {
		return printEmailDomainAdminError(err, 2)
	}
	var status *client.AgentEmailDomainRecoveryStatus
	switch action {
	case "status":
		if strings.TrimSpace(*sourceStream) != "" || *expectedSequence != 0 ||
			strings.TrimSpace(*expectedHash) != "" ||
			strings.TrimSpace(*expectedActionFence) != "" ||
			strings.TrimSpace(*actionKey) != "" {
			return printEmailDomainAdminError(
				fmt.Errorf("start/action flags are not valid for recovery status"), 2)
		}
		status, err = client.GetAdminAgentEmailDomainRecovery(
			context.Background(), endpoint, adminToken, recoveryToken, *recoveryID)
	case "start":
		if strings.TrimSpace(*sourceStream) == "" || *expectedSequence < 1 ||
			strings.TrimSpace(*expectedHash) == "" ||
			strings.TrimSpace(*expectedActionFence) != "" {
			return printEmailDomainAdminError(
				fmt.Errorf("recovery start requires --source-stream, --expected-sequence, and --expected-hash"), 2)
		}
		key, reason, mutationErr := common.exactMutation()
		if mutationErr != nil {
			return printEmailDomainAdminError(mutationErr, 2)
		}
		status, err = client.StartAdminAgentEmailDomainRecovery(
			context.Background(), endpoint, adminToken, recoveryToken,
			*recoveryID, *sourceStream, *expectedSequence, *expectedHash,
			reason, key)
	case "advance", "verify":
		if strings.TrimSpace(*sourceStream) != "" || *expectedSequence != 0 ||
			strings.TrimSpace(*expectedHash) != "" ||
			strings.TrimSpace(*expectedActionFence) == "" ||
			strings.TrimSpace(*actionKey) == "" {
			return printEmailDomainAdminError(
				fmt.Errorf("recovery %s requires --expected-action-fence and --idempotency-key", action), 2)
		}
		if action == "advance" {
			status, err = client.AdvanceAdminAgentEmailDomainRecovery(
				context.Background(), endpoint, adminToken, recoveryToken,
				*recoveryID, *actionKey, *expectedActionFence)
		} else {
			status, err = client.VerifyAdminAgentEmailDomainRecovery(
				context.Background(), endpoint, adminToken, recoveryToken,
				*recoveryID, *actionKey, *expectedActionFence)
		}
	}
	if err != nil {
		return printEmailDomainAdminError(err, 1)
	}
	if *common.json {
		return printJSON(status)
	}
	printEmailDomainRecoveryStatus(status)
	return 0
}

func printEmailDomainJournalStatus(status *client.AgentEmailDomainJournalStatus) {
	stream, hash := "-", "-"
	remoteHealthy := "-"
	if status.RemoteHeadHealthy != nil {
		remoteHealthy = fmt.Sprintf("%t", *status.RemoteHeadHealthy)
	}
	degradation := status.DegradationCode
	if degradation == "" {
		degradation = "-"
	}
	var sequence, registryRevision, auditSequence int64
	if status.Head != nil {
		stream, hash = status.Head.StreamID, status.Head.Hash
		sequence = status.Head.Sequence
		registryRevision = status.Head.RegistryRevision
		auditSequence = status.Head.AuditSequence
	}
	capacityUsed := emailDomainAdminInt64Pointer(status.Capacity.Used)
	capacityRemaining := emailDomainAdminInt64Pointer(status.Capacity.Remaining)
	capacityNear := emailDomainAdminBoolPointer(status.Capacity.NearLimit)
	capacityAt := emailDomainAdminBoolPointer(status.Capacity.AtLimit)
	capacityMax := "-"
	if status.Capacity.Max > 0 {
		capacityMax = fmt.Sprintf("%d", status.Capacity.Max)
	}
	w, flush := tableWriter(
		"enabled\trequired\thealthy\tpending\tforked\tremote_checked\tremote_healthy\tdegradation\tstream\tsequence\thash\tregistry_revision\taudit_sequence\tcapacity_ready\tcapacity_used\tcapacity_max\tcapacity_remaining\tcapacity_near_limit\tcapacity_at_limit\tcapacity_breakdown")
	_, _ = fmt.Fprintf(w, "%t\t%t\t%t\t%t\t%t\t%t\t%s\t%s\t%s\t%d\t%s\t%d\t%d\t%t\t%s\t%s\t%s\t%s\t%s\t%s\n",
		status.Enabled, status.Required, status.Healthy, status.Pending,
		status.Forked, status.RemoteHeadChecked,
		emailDomainAdminColumn(remoteHealthy),
		emailDomainAdminColumn(degradation),
		emailDomainAdminColumn(stream), sequence, emailDomainAdminColumn(hash),
		registryRevision, auditSequence, status.Capacity.Ready,
		emailDomainAdminColumn(capacityUsed),
		emailDomainAdminColumn(capacityMax),
		emailDomainAdminColumn(capacityRemaining),
		emailDomainAdminColumn(capacityNear),
		emailDomainAdminColumn(capacityAt),
		emailDomainAdminColumn(emailDomainAdminCapacityBreakdown(
			status.Capacity.Breakdown)))
	flush()
}

func emailDomainAdminInt64Pointer(value *int64) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *value)
}

func emailDomainAdminBoolPointer(value *bool) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf("%t", *value)
}

func emailDomainAdminCapacityBreakdown(
	value *client.AgentEmailDomainAuthorityCapacityBreakdown,
) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf(
		"meta=%d,audit=%d,domain=%d,idempotency=%d,lifecycle_fence=%d,"+
			"lifecycle_intent=%d,plan_fence=%d,plan_intent=%d,request=%d",
		value.Meta, value.Audit, value.Domain, value.Idempotency,
		value.LifecycleFence, value.LifecycleIntent, value.PlanFence,
		value.PlanIntent, value.Request,
	)
}

func printEmailDomainJournalProgress(progress *client.AgentEmailDomainJournalProgress) {
	stream, hash := "-", "-"
	var sequence int64
	if progress.Head != nil {
		stream, hash, sequence = progress.Head.StreamID, progress.Head.Hash,
			progress.Head.Sequence
	}
	w, flush := tableWriter(
		"kind\tphase\tcomplete\tfrozen\tauthority_keys\tscanned_keys\tstream\tsequence\thash")
	_, _ = fmt.Fprintf(w, "%s\t%s\t%t\t%t\t%d\t%d\t%s\t%d\t%s\n",
		emailDomainAdminColumn(progress.Kind), emailDomainAdminColumn(progress.Phase),
		progress.Complete, progress.Frozen, progress.AuthorityKeys,
		progress.ScannedKeys, emailDomainAdminColumn(stream), sequence,
		emailDomainAdminColumn(hash))
	flush()
}

func printEmailDomainRecoveryStatus(status *client.AgentEmailDomainRecoveryStatus) {
	w, flush := tableWriter(
		"recovery\tstream\tphase\tauthority_keys\tderived_keys\tsealed\tfailed\tfailure_code\taction_fence")
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%t\t%t\t%s\t%s\n",
		emailDomainAdminColumn(status.RecoveryID),
		emailDomainAdminColumn(status.SourceStream),
		emailDomainAdminColumn(status.Phase), status.AuthorityKeys,
		status.DerivedKeys, status.Sealed, status.Failed,
		emailDomainAdminColumn(status.FailureCode),
		emailDomainAdminColumn(status.ActionFence))
	flush()
}

func emailDomainAdminAudit(args []string) int {
	common := newEmailDomainAdminCommon("email-domain audit", false)
	action := common.flagSet.String("action", "", "filter by audit action")
	accountID := common.flagSet.String("account", "", "filter by account id")
	domain := common.flagSet.String("domain", "", "filter by exact domain")
	cursor := common.flagSet.String("cursor", "", "continue from an opaque next-page cursor")
	limit := common.flagSet.Int("limit", 100, "underlying audit rows to scan (1-100)")
	if err := common.flagSet.Parse(args); err != nil || common.flagSet.NArg() != 0 {
		return 2
	}
	if *limit < 1 || *limit > 100 {
		fmt.Fprintln(os.Stderr, "witself-admin: --limit must be between 1 and 100")
		return 2
	}
	endpoint, token, err := common.credentials()
	if err != nil {
		return printEmailDomainAdminError(err, 2)
	}
	page, err := client.ListAdminAgentEmailDomainAuditPage(
		context.Background(), endpoint, token,
		client.AdminAgentEmailDomainAuditFilter{
			Action: *action, AccountID: *accountID, Domain: *domain,
			Limit: *limit, Cursor: *cursor,
		})
	if err != nil {
		return printEmailDomainAdminError(err, 1)
	}
	if page.Events == nil {
		page.Events = []client.AgentEmailDomainAuditEvent{}
	}
	if *common.json {
		return printJSON(page)
	}
	w, flush := tableWriter("sequence\ttime\taction\ttarget\taccount\tdomain\tactor")
	for i := range page.Events {
		event := &page.Events[i]
		accountID, domain := emailDomainAdminAuditScope(event)
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s:%s\n",
			event.Sequence, emailDomainAdminTime(event.OccurredAt),
			emailDomainAdminColumn(event.Action),
			emailDomainAdminColumn(event.Target),
			emailDomainAdminColumn(accountID),
			emailDomainAdminColumn(domain),
			emailDomainAdminColumn(event.ActorKind),
			emailDomainAdminColumn(event.ActorID))
	}
	flush()
	printEmailDomainAdminNextCursor(page.NextCursor)
	return 0
}

func emailDomainAdminAuditScope(event *client.AgentEmailDomainAuditEvent) (string, string) {
	var accountID, domain string
	if len(event.Metadata) != 0 {
		var metadata struct {
			AccountID string `json:"account_id"`
			Domain    string `json:"domain"`
		}
		if json.Unmarshal(event.Metadata, &metadata) == nil {
			if accountID == "" {
				accountID = metadata.AccountID
			}
			if domain == "" {
				domain = metadata.Domain
			}
		}
	}
	if domain == "" && strings.Contains(event.Target, ".") {
		domain = event.Target
	}
	return accountID, domain
}

func printEmailDomainAdminRequest(
	request *client.AgentEmailDomainRequest, jsonOut, includeChallenge bool,
) int {
	if jsonOut {
		return printJSON(map[string]any{"request": request})
	}
	if includeChallenge {
		var recordName, recordType, recordValue string
		verification, lastResult := emailDomainAdminVerificationColumns(request)
		if request.OwnershipChallenge != nil {
			recordName = request.OwnershipChallenge.RecordName
			recordType = request.OwnershipChallenge.RecordType
			recordValue = request.OwnershipChallenge.RecordValue
		}
		w, flush := tableWriter(
			"id\taccount\tdomain\tstate\tavailability\tverification\tlast_result\trecord_name\trecord_type\trecord_value\tupdated")
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			emailDomainAdminColumn(request.RequestID),
			emailDomainAdminColumn(request.AccountID),
			emailDomainAdminColumn(request.Domain),
			emailDomainAdminColumn(request.State),
			emailDomainAdminColumn(request.Availability),
			emailDomainAdminColumn(verification),
			emailDomainAdminColumn(lastResult),
			emailDomainAdminColumn(recordName),
			emailDomainAdminColumn(recordType),
			emailDomainAdminColumn(recordValue),
			emailDomainAdminOptionalTime(request.UpdatedAt))
		flush()
		return 0
	}
	verification, lastResult := emailDomainAdminVerificationColumns(request)
	w, flush := tableWriter(
		"id\taccount\tdomain\tstate\tavailability\tverification\tlast_result")
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		emailDomainAdminColumn(request.RequestID),
		emailDomainAdminColumn(request.AccountID),
		emailDomainAdminColumn(request.Domain),
		emailDomainAdminColumn(request.State),
		emailDomainAdminColumn(request.Availability),
		emailDomainAdminColumn(verification),
		emailDomainAdminColumn(lastResult))
	flush()
	return 0
}

func emailDomainAdminVerificationColumns(
	request *client.AgentEmailDomainRequest,
) (string, string) {
	if request.OwnershipVerification == nil {
		return "", ""
	}
	return request.OwnershipVerification.State,
		request.OwnershipVerification.LastResult
}

func emailDomainAdminColumn(value string) string {
	return tabSafe(safeText(value))
}

func printEmailDomainAdminNextCursor(cursor string) {
	if cursor = strings.TrimSpace(cursor); cursor != "" {
		fmt.Fprintf(os.Stderr, "next cursor: %s\n", emailDomainAdminColumn(cursor))
	}
}

func printEmailDomainAdminError(err error, code int) int {
	fmt.Fprintf(os.Stderr, "witself-admin: %s\n",
		emailDomainAdminColumn(err.Error()))
	return code
}

func emailDomainAdminTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}

func emailDomainAdminOptionalTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return emailDomainAdminTime(*value)
}
