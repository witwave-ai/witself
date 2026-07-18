package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/witwave-ai/witself/internal/avatar"
	"github.com/witwave-ai/witself/internal/client"
)

const (
	avatarCommandUsage       = "usage: witself avatar show|history|version|style|propose|activate|rollback|reset|generation|operator ..."
	maxAvatarReasonCodeBytes = 128
)

var avatarReasonCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

func avatarCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, avatarCommandUsage)
		return 2
	}
	switch args[0] {
	case "show":
		return avatarShow(args[1:], false)
	case "history":
		return avatarHistory(args[1:], false)
	case "version":
		return avatarVersionShow(args[1:], false)
	case "style":
		return avatarSelfStyleCmd(args[1:])
	case "propose":
		return avatarPropose(args[1:], false)
	case "activate":
		return avatarActivate(args[1:], false)
	case "rollback":
		return avatarRollback(args[1:], false)
	case "reset":
		return avatarReset(args[1:], false)
	case "generation":
		return avatarGenerationCmd(args[1:])
	case "operator":
		return avatarOperatorCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself avatar: unknown subcommand %q\n", args[0])
		return 2
	}
}

func avatarSelfStyleCmd(args []string) int {
	if len(args) == 0 || args[0] != "show" {
		fmt.Fprintln(os.Stderr, "usage: witself avatar style show [agent connection flags]")
		return 2
	}
	return avatarSelfStyleShow(args[1:])
}

func avatarGenerationCmd(args []string) int {
	if len(args) == 0 || args[0] != "fail" {
		fmt.Fprintln(os.Stderr, "usage: witself avatar generation fail --expected-profile-revision N --reason-code CODE --idempotency-key KEY [agent connection flags]")
		return 2
	}
	return avatarGenerationFail(args[1:])
}

func avatarOperatorCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself avatar operator show|history|version|propose|activate|reject|rollback|reset|policy|style ...")
		return 2
	}
	switch args[0] {
	case "show":
		return avatarShow(args[1:], true)
	case "history":
		return avatarHistory(args[1:], true)
	case "version":
		return avatarVersionShow(args[1:], true)
	case "propose":
		return avatarPropose(args[1:], true)
	case "activate":
		return avatarActivate(args[1:], true)
	case "reject":
		return avatarReject(args[1:])
	case "rollback":
		return avatarRollback(args[1:], true)
	case "reset":
		return avatarReset(args[1:], true)
	case "policy":
		return avatarPolicy(args[1:])
	case "style":
		return avatarOperatorStyleCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself avatar operator: unknown subcommand %q\n", args[0])
		return 2
	}
}

func avatarOperatorStyleCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself avatar operator style show|version ...")
		return 2
	}
	switch args[0] {
	case "show":
		return avatarOperatorStyleShow(args[1:])
	case "version":
		return avatarOperatorStyleVersion(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself avatar operator style: unknown subcommand %q\n", args[0])
		return 2
	}
}

type avatarConnectionFlags struct {
	self        bool
	account     *string
	realm       *string
	localAgent  *string
	endpoint    *string
	tokenFile   *string
	targetAgent *string
	targetRealm *string
}

func addAvatarSelfConnectionFlags(fs *flag.FlagSet) avatarConnectionFlags {
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	return avatarConnectionFlags{
		self: true, account: account, realm: realm, localAgent: agent,
		endpoint: endpoint, tokenFile: tokenFile,
	}
}

func addAvatarOperatorConnectionFlags(fs *flag.FlagSet, target string) avatarConnectionFlags {
	flags := avatarConnectionFlags{
		account:   accountFlag(fs),
		endpoint:  fs.String("endpoint", "", "witself-server endpoint URL"),
		tokenFile: fs.String("token-file", "", "file containing an operator token"),
	}
	switch target {
	case "agent":
		flags.targetAgent = fs.String("agent-id", "", "target account agent id")
	case "realm":
		flags.targetRealm = fs.String("realm-id", "", "target account realm id")
	}
	return flags
}

func (f avatarConnectionFlags) connect(ctx context.Context) (string, string, error) {
	if f.self {
		conn, err := connectAgent(ctx, *f.account, *f.realm, *f.localAgent, *f.endpoint, *f.tokenFile)
		if err != nil {
			return "", "", err
		}
		return conn.Endpoint, conn.Token, nil
	}
	return connect(ctx, *f.account, *f.endpoint, *f.tokenFile)
}

func (f avatarConnectionFlags) agentID() string {
	if f.targetAgent == nil {
		return ""
	}
	return strings.TrimSpace(*f.targetAgent)
}

func (f avatarConnectionFlags) realmID() string {
	if f.targetRealm == nil {
		return ""
	}
	return strings.TrimSpace(*f.targetRealm)
}

func avatarShow(args []string, operator bool) int {
	name := "avatar show"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addAvatarSelfConnectionFlags(fs)
	if operator {
		name = "avatar operator show"
		fs = flag.NewFlagSet(name, flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		connFlags = addAvatarOperatorConnectionFlags(fs, "agent")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || (operator && connFlags.agentID() == "") {
		if operator {
			fmt.Fprintln(os.Stderr, "usage: witself avatar operator show --agent-id AGENT [operator connection flags]")
		} else {
			fmt.Fprintln(os.Stderr, "usage: witself avatar show [agent connection flags]")
		}
		return 2
	}
	ctx := context.Background()
	endpoint, token, err := connFlags.connect(ctx)
	if err != nil {
		return avatarCLIError(err)
	}
	var view *client.AvatarView
	if operator {
		view, err = client.GetAgentAvatar(ctx, endpoint, token, connFlags.agentID())
	} else {
		view, err = client.GetSelfAvatar(ctx, endpoint, token)
	}
	if err != nil {
		return avatarCLIError(err)
	}
	return printJSON(map[string]any{"avatar": view})
}

func avatarHistory(args []string, operator bool) int {
	name := "avatar history"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addAvatarSelfConnectionFlags(fs)
	if operator {
		name = "avatar operator history"
		fs = flag.NewFlagSet(name, flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		connFlags = addAvatarOperatorConnectionFlags(fs, "agent")
	}
	limit := fs.Int("limit", 20, "number of metadata summaries to return (1-100)")
	beforeVersion := fs.Int64("before-version", 0, "exclusive version cursor from next_before_version")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *limit < 1 || *limit > 100 || *beforeVersion < 0 || (operator && connFlags.agentID() == "") {
		if operator {
			fmt.Fprintln(os.Stderr, "usage: witself avatar operator history --agent-id AGENT [--limit 1-100] [--before-version N] [operator connection flags]")
		} else {
			fmt.Fprintln(os.Stderr, "usage: witself avatar history [--limit 1-100] [--before-version N] [agent connection flags]")
		}
		return 2
	}
	ctx := context.Background()
	endpoint, token, err := connFlags.connect(ctx)
	if err != nil {
		return avatarCLIError(err)
	}
	var history *client.AvatarHistoryPage
	opts := client.AvatarHistoryOptions{Limit: *limit, BeforeVersion: *beforeVersion}
	if operator {
		history, err = client.GetAgentAvatarHistoryPage(ctx, endpoint, token, connFlags.agentID(), opts)
	} else {
		history, err = client.GetSelfAvatarHistoryPage(ctx, endpoint, token, opts)
	}
	if err != nil {
		return avatarCLIError(err)
	}
	return printJSON(history)
}

func avatarVersionShow(args []string, operator bool) int {
	name := "avatar version"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addAvatarSelfConnectionFlags(fs)
	if operator {
		name = "avatar operator version"
		fs = flag.NewFlagSet(name, flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		connFlags = addAvatarOperatorConnectionFlags(fs, "agent")
	}
	version := fs.Int64("version", 0, "exact positive immutable avatar version")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *version < 1 || (operator && connFlags.agentID() == "") {
		if operator {
			fmt.Fprintln(os.Stderr, "usage: witself avatar operator version --agent-id AGENT --version N [operator connection flags]")
		} else {
			fmt.Fprintln(os.Stderr, "usage: witself avatar version --version N [agent connection flags]")
		}
		return 2
	}
	ctx := context.Background()
	endpoint, token, err := connFlags.connect(ctx)
	if err != nil {
		return avatarCLIError(err)
	}
	var avatarVersion *client.AvatarVersion
	if operator {
		avatarVersion, err = client.GetAgentAvatarVersion(ctx, endpoint, token, connFlags.agentID(), *version)
	} else {
		avatarVersion, err = client.GetSelfAvatarVersion(ctx, endpoint, token, *version)
	}
	if err != nil {
		return avatarCLIError(err)
	}
	return printJSON(map[string]any{"version": avatarVersion})
}

func avatarSelfStyleShow(args []string) int {
	fs := flag.NewFlagSet("avatar style show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addAvatarSelfConnectionFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself avatar style show [agent connection flags]")
		return 2
	}
	ctx := context.Background()
	endpoint, token, err := connFlags.connect(ctx)
	if err != nil {
		return avatarCLIError(err)
	}
	style, err := client.GetSelfAvatarStyle(ctx, endpoint, token)
	if err != nil {
		return avatarCLIError(err)
	}
	return printJSON(map[string]any{"style": style})
}

func avatarPropose(args []string, operator bool) int {
	name := "avatar propose"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addAvatarSelfConnectionFlags(fs)
	if operator {
		name = "avatar operator propose"
		fs = flag.NewFlagSet(name, flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		connFlags = addAvatarOperatorConnectionFlags(fs, "agent")
	}
	expectedRevision := fs.Int64("expected-profile-revision", 0, "exact current avatar profile revision")
	parentVersion := fs.Int64("parent-version", 0, "exact active parent version for an evolution")
	stylePackID := fs.String("style-pack-id", "", "selected immutable style pack id")
	stylePackVersion := fs.Int("style-pack-version", 0, "selected immutable style pack version")
	subjectForm := fs.String("subject-form", "", "human, animal, insect, anthropomorphic, hybrid, robot, or symbolic")
	description := fs.String("description", "", "bounded human-readable avatar description")
	specFile := fs.String("spec-file", "", "read visual specification JSON from FILE ('-' means stdin)")
	specStdin := fs.Bool("spec-stdin", false, "read visual specification JSON from stdin")
	svgFile := fs.String("svg-file", "", "read generated SVG from FILE ('-' means stdin)")
	svgStdin := fs.Bool("svg-stdin", false, "read generated SVG from stdin")
	runtimeName := fs.String("runtime", "", "self-reported generation runtime")
	model := fs.String("model", "", "self-reported generation model")
	recipe := fs.String("recipe", "", "self-reported generation recipe")
	recipeVersion := fs.String("recipe-version", "", "self-reported recipe version")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this one proposal")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	form := avatar.SubjectForm(strings.TrimSpace(*subjectForm))
	if fs.NArg() != 0 || *expectedRevision < 1 || *parentVersion < 0 ||
		strings.TrimSpace(*stylePackID) == "" || *stylePackVersion < 1 || form.Validate() != nil ||
		strings.TrimSpace(*description) == "" || strings.TrimSpace(*idempotencyKey) == "" ||
		(operator && connFlags.agentID() == "") {
		avatarProposeUsage(operator)
		return 2
	}
	if (*specStdin || strings.TrimSpace(*specFile) == "-") && (*svgStdin || strings.TrimSpace(*svgFile) == "-") {
		fmt.Fprintln(os.Stderr, "witself: visual spec and SVG cannot both read from stdin")
		return 2
	}
	spec, err := readAvatarPayload("visual spec", *specFile, *specStdin, avatar.MaxSpecJSONBytes, os.Stdin)
	if err != nil {
		return avatarCLIInputError(err)
	}
	spec, err = avatar.NormalizeSpecJSON(spec)
	if err != nil {
		return avatarCLIInputError(err)
	}
	svg, err := readAvatarPayload("SVG", *svgFile, *svgStdin, avatar.MaxSVGBytes, os.Stdin)
	if err != nil {
		return avatarCLIInputError(err)
	}
	descriptionValue, err := avatar.NormalizeDescription(*description)
	if err != nil {
		return avatarCLIInputError(err)
	}
	in := client.ProposeAvatarInput{
		ExpectedProfileRevision: *expectedRevision,
		ParentVersion:           *parentVersion,
		StylePackID:             strings.TrimSpace(*stylePackID),
		StylePackVersion:        *stylePackVersion,
		SubjectForm:             form,
		Description:             descriptionValue,
		VisualSpec:              json.RawMessage(spec),
		SVG:                     string(svg),
		Provenance: client.AvatarClientProvenance{
			Runtime: strings.TrimSpace(*runtimeName), Model: strings.TrimSpace(*model),
			Recipe: strings.TrimSpace(*recipe), RecipeVersion: strings.TrimSpace(*recipeVersion),
		},
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	}
	ctx := context.Background()
	endpoint, token, err := connFlags.connect(ctx)
	if err != nil {
		return avatarCLIError(err)
	}
	var result *client.AvatarMutationResult
	if operator {
		result, err = client.ProposeAgentAvatar(ctx, endpoint, token, connFlags.agentID(), in)
	} else {
		result, err = client.ProposeSelfAvatar(ctx, endpoint, token, in)
	}
	if err != nil {
		return avatarCLIError(err)
	}
	return printJSON(result)
}

func avatarProposeUsage(operator bool) {
	prefix := "witself avatar propose"
	selector := ""
	if operator {
		prefix = "witself avatar operator propose"
		selector = " --agent-id AGENT"
	}
	fmt.Fprintf(os.Stderr, "usage: %s%s --expected-profile-revision N --style-pack-id ID --style-pack-version N --subject-form FORM --description TEXT (--spec-file FILE|--spec-stdin) (--svg-file FILE|--svg-stdin) --idempotency-key KEY\n", prefix, selector)
}

func avatarActivate(args []string, operator bool) int {
	return avatarVersionMutation(args, operator, "activate")
}

func avatarRollback(args []string, operator bool) int {
	return avatarVersionMutation(args, operator, "rollback")
}

func avatarReset(args []string, operator bool) int {
	name := "avatar reset"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addAvatarSelfConnectionFlags(fs)
	if operator {
		name = "avatar operator reset"
		fs = flag.NewFlagSet(name, flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		connFlags = addAvatarOperatorConnectionFlags(fs, "agent")
	}
	expectedRevision := fs.Int64("expected-profile-revision", 0, "exact current avatar profile revision")
	reasonCode := fs.String("reason-code", "", "optional bounded machine-readable reset reason")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this reset")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	reason := strings.TrimSpace(*reasonCode)
	if fs.NArg() != 0 || *expectedRevision < 1 || !validAvatarReasonCode(reason, true) ||
		strings.TrimSpace(*idempotencyKey) == "" || (operator && connFlags.agentID() == "") {
		selector := ""
		if operator {
			selector = " --agent-id AGENT"
		}
		fmt.Fprintf(os.Stderr, "usage: witself avatar %sreset%s --expected-profile-revision N [--reason-code CODE] --idempotency-key KEY\n",
			map[bool]string{true: "operator ", false: ""}[operator], selector)
		return 2
	}
	ctx := context.Background()
	endpoint, token, err := connFlags.connect(ctx)
	if err != nil {
		return avatarCLIError(err)
	}
	in := client.ResetAvatarInput{
		ExpectedProfileRevision: *expectedRevision, ReasonCode: reason,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	}
	var result *client.AvatarMutationResult
	if operator {
		result, err = client.ResetAgentAvatar(ctx, endpoint, token, connFlags.agentID(), in)
	} else {
		result, err = client.ResetSelfAvatar(ctx, endpoint, token, in)
	}
	if err != nil {
		return avatarCLIError(err)
	}
	return printJSON(result)
}

func avatarVersionMutation(args []string, operator bool, action string) int {
	name := "avatar " + action
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addAvatarSelfConnectionFlags(fs)
	if operator {
		name = "avatar operator " + action
		fs = flag.NewFlagSet(name, flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		connFlags = addAvatarOperatorConnectionFlags(fs, "agent")
	}
	version := fs.Int64("version", 0, "exact immutable avatar version")
	expectedRevision := fs.Int64("expected-profile-revision", 0, "exact current avatar profile revision")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this lifecycle mutation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *version < 1 || *expectedRevision < 1 || strings.TrimSpace(*idempotencyKey) == "" ||
		(operator && connFlags.agentID() == "") {
		selector := ""
		if operator {
			selector = " --agent-id AGENT"
		}
		fmt.Fprintf(os.Stderr, "usage: witself avatar %s%s%s --version N --expected-profile-revision N --idempotency-key KEY\n",
			map[bool]string{true: "operator ", false: ""}[operator], action, selector)
		return 2
	}
	ctx := context.Background()
	endpoint, token, err := connFlags.connect(ctx)
	if err != nil {
		return avatarCLIError(err)
	}
	key := strings.TrimSpace(*idempotencyKey)
	var result *client.AvatarMutationResult
	switch {
	case action == "activate" && operator:
		result, err = client.ActivateAgentAvatar(ctx, endpoint, token, connFlags.agentID(), client.ActivateAvatarInput{
			Version: *version, ExpectedProfileRevision: *expectedRevision, IdempotencyKey: key,
		})
	case action == "activate":
		result, err = client.ActivateSelfAvatar(ctx, endpoint, token, client.ActivateAvatarInput{
			Version: *version, ExpectedProfileRevision: *expectedRevision, IdempotencyKey: key,
		})
	case operator:
		result, err = client.RollbackAgentAvatar(ctx, endpoint, token, connFlags.agentID(), client.RollbackAvatarInput{
			Version: *version, ExpectedProfileRevision: *expectedRevision, IdempotencyKey: key,
		})
	default:
		result, err = client.RollbackSelfAvatar(ctx, endpoint, token, client.RollbackAvatarInput{
			Version: *version, ExpectedProfileRevision: *expectedRevision, IdempotencyKey: key,
		})
	}
	if err != nil {
		return avatarCLIError(err)
	}
	return printJSON(result)
}

func avatarGenerationFail(args []string) int {
	fs := flag.NewFlagSet("avatar generation fail", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addAvatarSelfConnectionFlags(fs)
	expectedRevision := fs.Int64("expected-profile-revision", 0, "exact current avatar profile revision")
	reasonCode := fs.String("reason-code", "", "bounded machine-readable failure code")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this failed attempt")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	reason := strings.TrimSpace(*reasonCode)
	if fs.NArg() != 0 || *expectedRevision < 1 || !validAvatarReasonCode(reason, false) || strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself avatar generation fail --expected-profile-revision N --reason-code CODE --idempotency-key KEY [agent connection flags]")
		return 2
	}
	ctx := context.Background()
	endpoint, token, err := connFlags.connect(ctx)
	if err != nil {
		return avatarCLIError(err)
	}
	result, err := client.ReportSelfAvatarGenerationFailure(ctx, endpoint, token, client.AvatarGenerationFailureInput{
		ExpectedProfileRevision: *expectedRevision, ReasonCode: reason,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		return avatarCLIError(err)
	}
	return printJSON(result)
}

func avatarReject(args []string) int {
	fs := flag.NewFlagSet("avatar operator reject", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addAvatarOperatorConnectionFlags(fs, "agent")
	version := fs.Int64("version", 0, "exact pending avatar version")
	expectedRevision := fs.Int64("expected-profile-revision", 0, "exact current avatar profile revision")
	reasonCode := fs.String("reason-code", "", "optional bounded machine-readable rejection code")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this rejection")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	reason := strings.TrimSpace(*reasonCode)
	if fs.NArg() != 0 || connFlags.agentID() == "" || *version < 1 || *expectedRevision < 1 ||
		!validAvatarReasonCode(reason, true) || strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself avatar operator reject --agent-id AGENT --version N --expected-profile-revision N [--reason-code CODE] --idempotency-key KEY")
		return 2
	}
	ctx := context.Background()
	endpoint, token, err := connFlags.connect(ctx)
	if err != nil {
		return avatarCLIError(err)
	}
	result, err := client.RejectAgentAvatar(ctx, endpoint, token, connFlags.agentID(), client.RejectAvatarInput{
		Version: *version, ExpectedProfileRevision: *expectedRevision, ReasonCode: reason,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		return avatarCLIError(err)
	}
	return printJSON(result)
}

func avatarPolicy(args []string) int {
	fs := flag.NewFlagSet("avatar operator policy", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addAvatarOperatorConnectionFlags(fs, "agent")
	policyRaw := fs.String("policy", "", "operator_only, agent_proposes, or agent_self_managed")
	expectedRevision := fs.Int64("expected-profile-revision", 0, "exact current avatar profile revision")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this policy change")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	policy := avatar.AutonomyPolicy(strings.TrimSpace(*policyRaw))
	if fs.NArg() != 0 || connFlags.agentID() == "" || policy.Validate() != nil || *expectedRevision < 1 || strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself avatar operator policy --agent-id AGENT --policy POLICY --expected-profile-revision N --idempotency-key KEY")
		return 2
	}
	ctx := context.Background()
	endpoint, token, err := connFlags.connect(ctx)
	if err != nil {
		return avatarCLIError(err)
	}
	result, err := client.UpdateAgentAvatarPolicy(ctx, endpoint, token, connFlags.agentID(), client.UpdateAvatarPolicyInput{
		Policy: policy, ExpectedProfileRevision: *expectedRevision,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		return avatarCLIError(err)
	}
	return printJSON(result)
}

func avatarOperatorStyleShow(args []string) int {
	fs := flag.NewFlagSet("avatar operator style show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addAvatarOperatorConnectionFlags(fs, "realm")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || connFlags.realmID() == "" {
		fmt.Fprintln(os.Stderr, "usage: witself avatar operator style show --realm-id REALM [operator connection flags]")
		return 2
	}
	ctx := context.Background()
	endpoint, token, err := connFlags.connect(ctx)
	if err != nil {
		return avatarCLIError(err)
	}
	style, err := client.GetRealmAvatarStyle(ctx, endpoint, token, connFlags.realmID())
	if err != nil {
		return avatarCLIError(err)
	}
	return printJSON(map[string]any{"style": style})
}

func avatarOperatorStyleVersion(args []string) int {
	fs := flag.NewFlagSet("avatar operator style version", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addAvatarOperatorConnectionFlags(fs, "realm")
	expectedRevision := fs.Int64("expected-style-revision", 0, "exact current realm style revision")
	styleFile := fs.String("style-file", "", "read the complete style pack JSON from FILE ('-' means stdin)")
	styleStdin := fs.Bool("style-stdin", false, "read the complete style pack JSON from stdin")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this immutable style version")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || connFlags.realmID() == "" || *expectedRevision < 1 || strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself avatar operator style version --realm-id REALM --expected-style-revision N (--style-file FILE|--style-stdin) --idempotency-key KEY")
		return 2
	}
	raw, err := readAvatarPayload("style pack", *styleFile, *styleStdin, 2*1024*1024, os.Stdin)
	if err != nil {
		return avatarCLIInputError(err)
	}
	var stylePack avatar.StylePack
	if err := json.Unmarshal(raw, &stylePack); err != nil {
		return avatarCLIInputError(fmt.Errorf("style pack must contain one JSON object: %w", err))
	}
	if err := stylePack.Validate(); err != nil {
		return avatarCLIInputError(err)
	}
	ctx := context.Background()
	endpoint, token, err := connFlags.connect(ctx)
	if err != nil {
		return avatarCLIError(err)
	}
	result, err := client.CreateRealmAvatarStyleVersion(ctx, endpoint, token, connFlags.realmID(), client.CreateAvatarStyleVersionInput{
		ExpectedStyleRevision: *expectedRevision, StylePack: stylePack,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		return avatarCLIError(err)
	}
	return printJSON(result)
}

func readAvatarPayload(label, path string, stdin bool, maximum int, stdinReader io.Reader) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "-" {
		if stdin {
			return nil, fmt.Errorf("only one %s file or stdin source may be set", label)
		}
		stdin = true
		path = ""
	}
	if (path == "") == !stdin {
		return nil, fmt.Errorf("exactly one %s file or stdin source is required", label)
	}
	var reader io.Reader
	var closeFile *os.File
	if stdin {
		reader = stdinReader
	} else {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("read %s file: %w", label, err)
		}
		closeFile = file
		reader = file
	}
	if closeFile != nil {
		defer closeFile.Close()
	}
	limited := io.LimitReader(reader, int64(maximum)+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if len(raw) == 0 || len(raw) > maximum {
		return nil, fmt.Errorf("%s must contain 1-%d bytes", label, maximum)
	}
	return raw, nil
}

func validAvatarReasonCode(value string, optional bool) bool {
	if value == "" {
		return optional
	}
	return len(value) <= maxAvatarReasonCodeBytes && avatarReasonCodePattern.MatchString(value)
}

func avatarCLIInputError(err error) int {
	fmt.Fprintf(os.Stderr, "witself: invalid avatar input: %v\n", err)
	return 2
}

func avatarCLIError(err error) int {
	fmt.Fprintf(os.Stderr, "witself: %v\n", err)
	return 1
}
