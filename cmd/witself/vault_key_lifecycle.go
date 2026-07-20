package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/sealed"
	"github.com/witwave-ai/witself/internal/secretclient"
)

const (
	minimumVaultEnrollmentTTL = time.Minute
	maximumVaultEnrollmentTTL = time.Hour
)

// vaultTerminal is deliberately the controlling terminal, not stdin or
// stdout. Pairing credentials and recovery passphrases must never be accepted
// through argv, environment variables, pipes, JSON, or an AI tool transcript.
type vaultTerminal struct {
	file *os.File
}

type vaultKeyRotationFileRecoverySink struct {
	path string
}

func (sink vaultKeyRotationFileRecoverySink) PutIfAbsent(ctx context.Context, _ sealed.AVKRecoveryMetadata, artifact []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := local.WriteRecoveryArtifact(sink.path, artifact); err != nil {
		if errors.Is(err, local.ErrAgentVaultKeyRecoveryExists) {
			return secretclient.ErrVaultKeyRotationRecoveryExists
		}
		return err
	}
	return nil
}

func (sink vaultKeyRotationFileRecoverySink) ReadBack(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	artifact, _, err := local.ReadRecoveryArtifact(sink.path)
	if errors.Is(err, local.ErrAgentVaultKeyRecoveryUnavailable) {
		return nil, secretclient.ErrVaultKeyRotationRecoveryUnavailable
	}
	return artifact, err
}

func openVaultTerminal() (*vaultTerminal, error) {
	file, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil || !term.IsTerminal(file.Fd()) {
		if file != nil {
			_ = file.Close()
		}
		return nil, errors.New("a controlling terminal is required for vault key credentials")
	}
	return &vaultTerminal{file: file}, nil
}

func (terminal *vaultTerminal) Close() {
	if terminal != nil && terminal.file != nil {
		_ = terminal.file.Close()
	}
}

func (terminal *vaultTerminal) readSecret(prompt string) ([]byte, error) {
	if terminal == nil || terminal.file == nil {
		return nil, errors.New("a controlling terminal is required for vault key credentials")
	}
	if _, err := fmt.Fprint(terminal.file, prompt); err != nil {
		return nil, errors.New("write vault key credential prompt")
	}
	value, err := term.ReadPassword(terminal.file.Fd())
	_, _ = fmt.Fprintln(terminal.file)
	if err != nil {
		clear(value)
		return nil, errors.New("read vault key credential from controlling terminal")
	}
	return value, nil
}

func (terminal *vaultTerminal) showPairingSecret(enrollmentID, pairingSecret, sas string) error {
	if terminal == nil || terminal.file == nil {
		return errors.New("a controlling terminal is required for vault key credentials")
	}
	_, err := fmt.Fprintf(terminal.file,
		"\nVault key enrollment %s\nPairing secret: %s\nVerification code: %s\n\n"+
			"Enter the pairing secret only at the already-enrolled installation.\n",
		enrollmentID, pairingSecret, sas)
	if err != nil {
		return errors.New("write vault key pairing credential to controlling terminal")
	}
	return nil
}

func vaultKeyEnroll(args []string) int {
	if len(args) == 0 || commandHelpRequested(args) {
		printCommandGroupHelp(os.Stderr,
			"usage: witself vault key enroll begin|approve|complete|list|status|cancel ...",
			"begin     Create a short-lived request on the installation that needs the key",
			"approve   Encrypt the current key to a request from an enrolled installation",
			"complete  Install the approved key and irreversibly consume the transfer",
			"list      List value-free enrollment lifecycle records",
			"status    Show one value-free enrollment lifecycle record",
			"cancel    Cancel a pending or approved enrollment",
		)
		if commandHelpRequested(args) {
			return 0
		}
		return 2
	}
	switch args[0] {
	case "begin":
		return vaultKeyEnrollBegin(args[1:])
	case "approve":
		return vaultKeyEnrollApprove(args[1:])
	case "complete":
		return vaultKeyEnrollComplete(args[1:])
	case "list":
		return vaultKeyEnrollList(args[1:])
	case "status", "cancel":
		return vaultKeyEnrollRecord(args[0], args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself: unknown vault key enroll command %q\n", args[0])
		return 2
	}
}

func vaultKeyEnrollBegin(args []string) int {
	fs := flag.NewFlagSet("vault key enroll begin", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, "usage: witself vault key enroll begin [--location NAME] [--ttl DURATION] [agent connection flags]")
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	location := fs.String("location", "", "stable lowercase installation label")
	ttl := fs.Duration("ttl", 10*time.Minute, "request lifetime from 1m through 1h")
	jsonOut := jsonFlag(fs)
	if parsed, exitCode := parseCommandFlags(fs, args); !parsed {
		return exitCode
	}
	if fs.NArg() != 0 || *ttl < minimumVaultEnrollmentTTL || *ttl > maximumVaultEnrollmentTTL {
		fmt.Fprintln(os.Stderr, "usage: witself vault key enroll begin [--location NAME] [--ttl DURATION] [agent connection flags]")
		return 2
	}
	terminal, err := openVaultTerminal()
	if err != nil {
		return printSecretCLIError("begin vault key enrollment", err)
	}
	defer terminal.Close()
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect vault", err)
	}
	result, err := cli.service.BeginVaultKeyEnrollment(context.Background(), secretclient.BeginVaultKeyEnrollmentInput{
		LocationName: *location,
		ExpiresAt:    time.Now().Add(*ttl),
	})
	if err != nil {
		return printSecretCLIError("begin vault key enrollment", err)
	}
	if err := terminal.showPairingSecret(result.Enrollment.ID, result.PairingSecret, result.SAS); err != nil {
		return printSecretCLIError("show vault key pairing credential", err)
	}
	if *jsonOut {
		return printJSON(map[string]any{
			"enrollment": result.Enrollment, "sas": result.SAS,
			"pairing_secret_delivery": "controlling_terminal",
		})
	}
	fmt.Printf("enrollment:\t%s\nstate:\t%s\nexpires:\t%s\nverification code:\t%s\npairing secret:\tshown on controlling terminal\n",
		result.Enrollment.ID, result.Enrollment.LifecycleState,
		result.Enrollment.ExpiresAt.UTC().Format(time.RFC3339), result.SAS)
	return 0
}

func vaultKeyEnrollApprove(args []string) int {
	fs := flag.NewFlagSet("vault key enroll approve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, "usage: witself vault key enroll approve ENROLLMENT_ID [--location NAME] [agent connection flags]")
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	location := fs.String("location", "", "stable lowercase source installation label")
	jsonOut := jsonFlag(fs)
	args = secretFlagParseOrder(args, 1)
	if parsed, exitCode := parseCommandFlags(fs, args); !parsed {
		return exitCode
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: witself vault key enroll approve ENROLLMENT_ID [--location NAME] [agent connection flags]")
		return 2
	}
	terminal, err := openVaultTerminal()
	if err != nil {
		return printSecretCLIError("approve vault key enrollment", err)
	}
	defer terminal.Close()
	pairing, err := terminal.readSecret("Enrollment pairing secret: ")
	if err != nil {
		return printSecretCLIError("approve vault key enrollment", err)
	}
	defer clear(pairing)
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect vault", err)
	}
	value, err := cli.service.ApproveVaultKeyEnrollment(context.Background(), secretclient.ApproveVaultKeyEnrollmentInput{
		EnrollmentID: fs.Arg(0), PairingSecret: strings.TrimSpace(string(pairing)), SourceLocationName: *location,
	})
	if err != nil {
		return printSecretCLIError("approve vault key enrollment", err)
	}
	return printVaultKeyEnrollmentMutation("approved", value, *jsonOut)
}

func vaultKeyEnrollComplete(args []string) int {
	fs := flag.NewFlagSet("vault key enroll complete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, "usage: witself vault key enroll complete ENROLLMENT_ID [--location NAME] [agent connection flags]")
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	location := fs.String("location", "", "stable lowercase target installation label")
	jsonOut := jsonFlag(fs)
	args = secretFlagParseOrder(args, 1)
	if parsed, exitCode := parseCommandFlags(fs, args); !parsed {
		return exitCode
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: witself vault key enroll complete ENROLLMENT_ID [--location NAME] [agent connection flags]")
		return 2
	}
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect vault", err)
	}
	value, err := cli.service.CompleteVaultKeyEnrollment(context.Background(), secretclient.CompleteVaultKeyEnrollmentInput{
		EnrollmentID: fs.Arg(0), TargetLocationName: *location,
	})
	if err != nil {
		return printSecretCLIError("complete vault key enrollment", err)
	}
	return printVaultKeyEnrollmentMutation("consumed", value, *jsonOut)
}

func vaultKeyEnrollList(args []string) int {
	fs := flag.NewFlagSet("vault key enroll list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, "usage: witself vault key enroll list [--state STATE] [--limit N] [agent connection flags]")
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	state := fs.String("state", "", "pending, approved, consumed, cancelled, or expired")
	limit := fs.Int("limit", 25, "maximum records (1-100)")
	jsonOut := jsonFlag(fs)
	if parsed, exitCode := parseCommandFlags(fs, args); !parsed {
		return exitCode
	}
	if fs.NArg() != 0 || *limit < 1 || *limit > 100 {
		fmt.Fprintln(os.Stderr, "usage: witself vault key enroll list [--state STATE] [--limit N] [agent connection flags]")
		return 2
	}
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect vault", err)
	}
	items, err := cli.service.ListVaultKeyEnrollments(context.Background(), client.VaultKeyEnrollmentListOptions{
		State: strings.TrimSpace(*state), Limit: *limit,
	})
	if err != nil {
		return printSecretCLIError("list vault key enrollments", err)
	}
	if *jsonOut {
		return printJSON(map[string]any{"items": items})
	}
	w, flush := tableWriter("id\tstate\ttarget\texpires (UTC)\tkey version")
	defer flush()
	for _, value := range items {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", value.ID, value.LifecycleState,
			tabSafe(safeText(value.TargetLocationName)), value.ExpiresAt.UTC().Format(time.RFC3339), value.VaultKeyVersion)
	}
	return 0
}

func vaultKeyEnrollRecord(operation string, args []string) int {
	fs := flag.NewFlagSet("vault key enroll "+operation, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, "usage: witself vault key enroll "+operation+" ENROLLMENT_ID [agent connection flags]")
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	jsonOut := jsonFlag(fs)
	args = secretFlagParseOrder(args, 1)
	if parsed, exitCode := parseCommandFlags(fs, args); !parsed {
		return exitCode
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: witself vault key enroll "+operation+" ENROLLMENT_ID [agent connection flags]")
		return 2
	}
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect vault", err)
	}
	var value *client.VaultKeyEnrollment
	if operation == "cancel" {
		value, err = cli.service.CancelVaultKeyEnrollment(context.Background(), fs.Arg(0))
	} else {
		value, err = cli.service.GetVaultKeyEnrollment(context.Background(), fs.Arg(0))
	}
	if err != nil {
		return printSecretCLIError(operation+" vault key enrollment", err)
	}
	return printVaultKeyEnrollmentMutation(operation, value, *jsonOut)
}

func printVaultKeyEnrollmentMutation(operation string, value *client.VaultKeyEnrollment, jsonOut bool) int {
	if jsonOut {
		return printJSON(map[string]any{"enrollment": value})
	}
	fmt.Printf("%s\t%s\t%s\t%s\n", operation, value.ID, value.LifecycleState,
		value.ExpiresAt.UTC().Format(time.RFC3339))
	return 0
}

func vaultKeyRecovery(args []string) int {
	if len(args) == 0 || commandHelpRequested(args) {
		printCommandGroupHelp(os.Stderr,
			"usage: witself vault key recovery export|inspect|import ...",
			"export   Write a passphrase-encrypted artifact for the current exact key epoch",
			"inspect  Inspect public artifact metadata without decrypting it",
			"import   Restore the exact epoch only when it matches the backend binding",
		)
		if commandHelpRequested(args) {
			return 0
		}
		return 2
	}
	switch args[0] {
	case "export", "inspect", "import":
		return vaultKeyRecoveryOperation(args[0], args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself: unknown vault key recovery command %q\n", args[0])
		return 2
	}
}

func vaultKeyRecoveryOperation(operation string, args []string) int {
	fs := flag.NewFlagSet("vault key recovery "+operation, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	usage := "usage: witself vault key recovery " + operation + " --file FILE"
	switch operation {
	case "export":
		usage = "usage: witself vault key recovery export --out FILE [agent connection flags]"
	case "import":
		usage += " [agent connection flags]"
	}
	configureCommandUsage(fs, usage)
	var account, realm, agent, endpoint, tokenFile *string
	if operation != "inspect" {
		account, realm, agent, endpoint, tokenFile = factConnectionFlags(fs)
	}
	file := fs.String("file", "", "recovery artifact to read")
	out := fs.String("out", "", "new recovery artifact path (never overwritten)")
	jsonOut := jsonFlag(fs)
	if parsed, exitCode := parseCommandFlags(fs, args); !parsed {
		return exitCode
	}
	validPath := operation == "export" && *out != "" && *file == "" ||
		operation != "export" && *file != "" && *out == ""
	if fs.NArg() != 0 || !validPath {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if operation == "inspect" {
		artifact, metadata, err := local.ReadRecoveryArtifact(*file)
		clear(artifact)
		if err != nil {
			return printSecretCLIError("inspect vault key recovery artifact", err)
		}
		return printVaultKeyRecoveryMetadata("inspected", metadata, *jsonOut)
	}
	terminal, err := openVaultTerminal()
	if err != nil {
		return printSecretCLIError(operation+" vault key recovery artifact", err)
	}
	defer terminal.Close()
	passphrase, err := terminal.readSecret("Recovery passphrase: ")
	if err != nil {
		return printSecretCLIError(operation+" vault key recovery artifact", err)
	}
	defer clear(passphrase)
	if operation == "export" {
		confirmation, err := terminal.readSecret("Confirm recovery passphrase: ")
		if err != nil {
			return printSecretCLIError("export vault key recovery artifact", err)
		}
		matches := len(passphrase) == len(confirmation) && subtle.ConstantTimeCompare(passphrase, confirmation) == 1
		clear(confirmation)
		if !matches {
			return printSecretCLIError("export vault key recovery artifact", errors.New("recovery passphrases do not match"))
		}
	}
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect vault", err)
	}
	if operation == "export" {
		artifact, metadata, err := cli.service.ExportVaultKeyRecovery(context.Background(), passphrase)
		if err != nil {
			clear(artifact)
			return printSecretCLIError("export vault key recovery artifact", err)
		}
		defer clear(artifact)
		if err := local.WriteRecoveryArtifact(*out, artifact); err != nil {
			return printSecretCLIError("write vault key recovery artifact", err)
		}
		if *jsonOut {
			return printJSON(map[string]any{"path": *out, "metadata": metadata})
		}
		fmt.Printf("exported\t%s\t%s\t%d\n", *out, metadata.AVK.ID, metadata.AVK.Version)
		return 0
	}
	artifact, _, err := local.ReadRecoveryArtifact(*file)
	if err != nil {
		return printSecretCLIError("read vault key recovery artifact", err)
	}
	defer clear(artifact)
	metadata, err := cli.service.ImportVaultKeyRecovery(context.Background(), artifact, passphrase)
	if err != nil {
		return printSecretCLIError("import vault key recovery artifact", err)
	}
	if *jsonOut {
		return printJSON(map[string]any{"state": "restored", "key": metadata})
	}
	fmt.Printf("restored\t%s\t%d\t%s\n", metadata.ID, metadata.Version, metadata.Fingerprint)
	return 0
}

func printVaultKeyRecoveryMetadata(operation string, metadata sealed.AVKRecoveryMetadata, jsonOut bool) int {
	if jsonOut {
		return printJSON(map[string]any{"state": operation, "metadata": metadata})
	}
	fmt.Printf("%s\t%s\t%d\t%s\t%s\n", operation, metadata.AVK.ID, metadata.AVK.Version,
		metadata.AVK.Fingerprint, metadata.KDFAlgorithm)
	return 0
}

func vaultKeyRotation(command string, args []string) int {
	if command == "rotation" {
		if len(args) == 0 || commandHelpRequested(args) {
			printCommandGroupHelp(os.Stderr,
				"usage: witself vault key rotation status|cancel [ROTATION_ID] [agent connection flags]",
				"status  Show one exact rotation, or the current open rotation when ID is omitted",
				"cancel  Cancel one open rotation, or the current open rotation when ID is omitted",
			)
			if commandHelpRequested(args) {
				return 0
			}
			return 2
		}
		if args[0] != "status" && args[0] != "cancel" {
			fmt.Fprintf(os.Stderr, "witself: unknown vault key rotation command %q\n", args[0])
			return 2
		}
		return vaultKeyRotationRecord(args[0], args[1:])
	}

	fs := flag.NewFlagSet("vault key rotate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	usage := "usage: witself vault key rotate (--recovery-out FILE|--accept-unrecoverable-key-loss) [agent connection flags]"
	configureCommandUsage(fs, usage)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	recoveryOut := fs.String("recovery-out", "", "durably write and verify the target-key recovery artifact before commit")
	acceptUnrecoverable := fs.Bool("accept-unrecoverable-key-loss", false, "commit without a recovery artifact and accept permanent sealed-plane loss risk")
	jsonOut := jsonFlag(fs)
	if parsed, exitCode := parseCommandFlags(fs, args); !parsed {
		return exitCode
	}
	*recoveryOut = strings.TrimSpace(*recoveryOut)
	hasRecoveryOut := *recoveryOut != ""
	if fs.NArg() != 0 || hasRecoveryOut == *acceptUnrecoverable {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	options := secretclient.RotateVaultKeyOptions{AcceptUnrecoverableKeyLoss: *acceptUnrecoverable}
	if *recoveryOut != "" {
		terminal, err := openVaultTerminal()
		if err != nil {
			return printSecretCLIError("prepare vault key rotation recovery", err)
		}
		defer terminal.Close()
		passphrase, err := terminal.readSecret("Recovery passphrase: ")
		if err != nil {
			return printSecretCLIError("prepare vault key rotation recovery", err)
		}
		defer clear(passphrase)
		confirmation, err := terminal.readSecret("Confirm recovery passphrase: ")
		if err != nil {
			return printSecretCLIError("prepare vault key rotation recovery", err)
		}
		matches := len(passphrase) == len(confirmation) && subtle.ConstantTimeCompare(passphrase, confirmation) == 1
		clear(confirmation)
		if !matches {
			return printSecretCLIError("prepare vault key rotation recovery", errors.New("recovery passphrases do not match"))
		}
		if len(passphrase) < sealed.MinAVKRecoveryPassphraseBytes || len(passphrase) > sealed.MaxAVKRecoveryPassphraseBytes {
			return printSecretCLIError("prepare vault key rotation recovery", errors.New("recovery passphrase length is invalid"))
		}
		options.RecoverySink = vaultKeyRotationFileRecoverySink{path: *recoveryOut}
		options.RecoveryPassphrase = passphrase
	}
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect vault", err)
	}
	rotation, err := cli.service.RotateVaultKey(context.Background(), options)
	if err != nil {
		return printSecretCLIError("rotate vault key", err)
	}
	code := printVaultKeyRotation("rotated", rotation, *jsonOut)
	if code != 0 {
		return code
	}
	if !*jsonOut && rotation != nil && rotation.LifecycleState == client.VaultKeyRotationCommitted {
		switch rotation.RecoveryDispositionMode {
		case client.VaultKeyRotationRecoveryArtifact:
			if _, err := fmt.Fprintf(os.Stderr, "recovery artifact verified before commit (sha256 %s)\n", rotation.RecoveryArtifactSHA256); err != nil {
				return printSecretCLIError("write vault key rotation guidance", err)
			}
		case client.VaultKeyRotationRiskAccepted:
			if _, err := fmt.Fprintln(os.Stderr, "warning: rotation committed after explicit acceptance of permanent key-loss risk"); err != nil {
				return printSecretCLIError("write vault key rotation guidance", err)
			}
		}
		if _, err := fmt.Fprintln(os.Stderr, "other installations must enroll again before they can use the rotated vault"); err != nil {
			return printSecretCLIError("write vault key rotation guidance", err)
		}
	}
	if rotation != nil && (rotation.LifecycleState == client.VaultKeyRotationCommitted ||
		rotation.LifecycleState == client.VaultKeyRotationCancelled) {
		if err := cli.service.AcknowledgeVaultKeyRotation(context.Background(), rotation.ID); err != nil {
			return printSecretCLIError("acknowledge vault key rotation", err)
		}
	}
	return 0
}

func vaultKeyRotationRecord(operation string, args []string) int {
	fs := flag.NewFlagSet("vault key rotation "+operation, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	usage := "usage: witself vault key rotation " + operation + " [ROTATION_ID] [agent connection flags]"
	configureCommandUsage(fs, usage)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	jsonOut := jsonFlag(fs)
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = secretFlagParseOrder(args, 1)
	}
	if parsed, exitCode := parseCommandFlags(fs, args); !parsed {
		return exitCode
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	cli, err := connectSecretCLI(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return printSecretCLIError("connect vault", err)
	}
	rotationID := ""
	if fs.NArg() == 1 {
		rotationID = fs.Arg(0)
	}
	var rotation *client.VaultKeyRotation
	if operation == "cancel" {
		rotation, err = cli.service.CancelVaultKeyRotation(context.Background(), rotationID)
	} else if rotationID == "" {
		rotation, err = cli.service.OpenVaultKeyRotation(context.Background())
	} else {
		rotation, err = cli.service.VaultKeyRotationStatus(context.Background(), rotationID)
	}
	if err != nil {
		return printSecretCLIError(operation+" vault key rotation", err)
	}
	code := printVaultKeyRotation(operation, rotation, *jsonOut)
	if code != 0 || operation != "cancel" || rotation == nil ||
		(rotation.LifecycleState != client.VaultKeyRotationCommitted &&
			rotation.LifecycleState != client.VaultKeyRotationCancelled) {
		return code
	}
	if err := cli.service.AcknowledgeVaultKeyRotation(context.Background(), rotation.ID); err != nil {
		return printSecretCLIError("acknowledge vault key rotation cancellation", err)
	}
	return 0
}

func printVaultKeyRotation(operation string, rotation *client.VaultKeyRotation, jsonOut bool) int {
	if jsonOut {
		return printJSON(map[string]any{"operation": operation, "rotation": rotation})
	}
	if rotation == nil {
		if _, err := fmt.Println("state:\tnone"); err != nil {
			return 1
		}
		return 0
	}
	if _, err := fmt.Printf("rotation:\t%s\nstate:\t%s\nsource key:\t%s (version %d)\ntarget key:\t%s (version %d)\nitems:\t%d/%d staged\nrevision:\t%d\n",
		rotation.ID, rotation.LifecycleState, rotation.SourceKeyID, rotation.SourceKeyVersion,
		rotation.TargetKeyID, rotation.TargetKeyVersion, rotation.StagedCount, rotation.ItemCount,
		rotation.RowVersion); err != nil {
		return 1
	}
	if rotation.LifecycleState == client.VaultKeyRotationCommitted {
		if _, err := fmt.Printf("recovery disposition:\t%s\n", rotation.RecoveryDispositionMode); err != nil {
			return 1
		}
		if rotation.RecoveryDispositionMode == client.VaultKeyRotationRecoveryArtifact {
			if _, err := fmt.Printf("recovery artifact sha256:\t%s\n", rotation.RecoveryArtifactSHA256); err != nil {
				return 1
			}
		}
	}
	return 0
}
