package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon"
)

var daemonSourceCmd = &cobra.Command{
	Use:   "source",
	Short: "Provision and approve a dedicated native source",
}

var daemonSourceProvisionCmd = &cobra.Command{
	Use:   "provision",
	Short: "Create a fresh native source and its local ownership marker",
	Long:  "Creates a disabled native source and a fresh machine-global domain. Optional --beads-executable initializes a fresh domain with Beads 1.3.1 on Linux before daemon adoption. It never enables execution or repairs an existing domain. Restart the daemon after provisioning so it can hold the domain ownership lock, then approve the source.",
	Args:  cobra.NoArgs,
	RunE:  runDaemonSourceProvision,
}

var daemonSourceApproveCmd = &cobra.Command{
	Use:   "approve",
	Short: "Stage a short-lived approval for the running daemon",
	Long:  "Requires the current workspace owner/admin to own the exact runtime. The local daemon must already hold the provisioned domain lock. Approval expires within 120 seconds. Enrollment alone does not initialize Beads or enable execution.",
	Args:  cobra.NoArgs,
	RunE:  runDaemonSourceApprove,
}

var daemonSourceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the server enrollment receipt",
	Args:  cobra.NoArgs,
	RunE:  runDaemonSourceStatus,
}

func init() {
	daemonCmd.AddCommand(daemonSourceCmd)
	daemonSourceCmd.AddCommand(daemonSourceProvisionCmd, daemonSourceApproveCmd, daemonSourceStatusCmd)
	daemonSourceProvisionCmd.Flags().String("beads-executable", "", "Explicit absolute Beads 1.3.1 executable for fresh Linux initialization")
	daemonSourceCmd.PersistentFlags().String("workspace", "", "Workspace UUID")
	daemonSourceCmd.PersistentFlags().String("daemon-id", "", "Daemon identity override, matching daemon start")
	_ = daemonSourceCmd.MarkPersistentFlagRequired("workspace")
	for _, key := range []string{"runtime", "name", "request-id"} {
		daemonSourceProvisionCmd.Flags().String(key, "", map[string]string{
			"runtime": "Owned runtime UUID", "name": "Source name", "request-id": "Stable request UUID for retries",
		}[key])
		_ = daemonSourceProvisionCmd.MarkFlagRequired(key)
	}
	for _, cmd := range []*cobra.Command{daemonSourceApproveCmd, daemonSourceStatusCmd} {
		cmd.Flags().String("source", "", "Native source UUID")
		_ = cmd.MarkFlagRequired("source")
	}
}

// Enrollment must never resolve the user's global config from a managed task,
// nor use LoadConfig, which probes installed agent executables.
func nativeSourceCLIClient(cmd *cobra.Command) (*daemon.Client, string, string, string, error) {
	if strings.TrimSpace(os.Getenv(cli.TaskConfigRootEnv)) != "" {
		return nil, "", "", "", fmt.Errorf("native source enrollment is not available inside a daemon-managed task")
	}
	if err := requireHumanLocalCommand("native source enrollment"); err != nil {
		return nil, "", "", "", err
	}
	profile := resolveProfile(cmd)
	baseURL, err := daemon.NormalizeServerBaseURL(tryResolveHumanServerURL(cmd))
	if err != nil {
		return nil, "", "", "", fmt.Errorf("configure a valid server URL before native source enrollment")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil {
		return nil, "", "", "", fmt.Errorf("native source enrollment requires a server URL without embedded credentials")
	}
	token := resolveToken(cmd)
	if token == "" {
		return nil, "", "", "", fmt.Errorf("authenticate as the workspace owner/admin and runtime owner first")
	}
	daemonID, _ := cmd.Flags().GetString("daemon-id")
	if daemonID == "" {
		daemonID = strings.TrimSpace(os.Getenv("MULTICA_DAEMON_ID"))
	}
	if daemonID == "" {
		daemonID, err = daemon.EnsureDaemonID(profile)
		if err != nil {
			return nil, "", "", "", err
		}
	}
	client := daemon.NewClient(baseURL)
	client.SetToken(token)
	return client, profile, baseURL, daemonID, nil
}

func runDaemonSourceProvision(cmd *cobra.Command, _ []string) error {
	client, profile, baseURL, daemonID, err := nativeSourceCLIClient(cmd)
	if err != nil {
		return err
	}
	executable, _ := cmd.Flags().GetString("beads-executable")
	if executable != "" && (!filepath.IsAbs(executable) || filepath.Clean(executable) != executable || runtime.GOOS != "linux") {
		return fmt.Errorf("native initialization requires an absolute Beads 1.3.1 executable on Linux")
	}
	workspaceID, _ := cmd.Flags().GetString("workspace")
	runtimeID, _ := cmd.Flags().GetString("runtime")
	requestID, _ := cmd.Flags().GetString("request-id")
	name, _ := cmd.Flags().GetString("name")
	source, err := client.CreateNativeSourceIntent(cmd.Context(), runtimeID, workspaceID, daemonID, requestID, name)
	if err != nil {
		return err
	}
	if err := daemon.ProvisionNativeSourceLocal(cmd.Context(), profile, baseURL, source, executable); err != nil {
		return fmt.Errorf("server intent exists, but local provisioning or initialization failed. Keep the same request UUID and do not reset the domain: %w", err)
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(source)
}

func runDaemonSourceApprove(cmd *cobra.Command, _ []string) error {
	client, profile, baseURL, daemonID, err := nativeSourceCLIClient(cmd)
	if err != nil {
		return err
	}
	workspaceID, _ := cmd.Flags().GetString("workspace")
	sourceID, _ := cmd.Flags().GetString("source")
	local, err := daemon.LoadNativeSourceEnrollmentLocal(profile, baseURL, workspaceID, sourceID)
	if err != nil {
		return err
	}
	if local.Identity.DaemonID != daemonID {
		return fmt.Errorf("local source belongs to a different daemon identity")
	}
	source, err := client.GetNativeSourceEnrollment(cmd.Context(), workspaceID, sourceID)
	if err != nil {
		return err
	}
	if source.RuntimeID != local.Identity.RuntimeID || source.DaemonID != daemonID || source.NativeEnrollmentID != local.Identity.EnrollmentID {
		return fmt.Errorf("server enrollment does not match the local ownership marker")
	}
	proof := daemon.NativeSourceEnrollmentProof{
		EnrollmentID: local.Identity.EnrollmentID, ConfigRevision: source.ConfigRevision, ManifestHash: local.ManifestHash,
	}
	credential, err := client.MintSourceEnrollmentToken(cmd.Context(), source.RuntimeID, workspaceID, daemonID, sourceID, proof)
	if err != nil {
		return err
	}
	if err := daemon.StageNativeSourceEnrollmentApproval(profile, baseURL, source, credential); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Approval staged for source %s until %s. Check 'multica daemon source status'; the running daemon must hold the source lock.\n", sourceID, credential.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"))
	return err
}

func runDaemonSourceStatus(cmd *cobra.Command, _ []string) error {
	client, _, _, _, err := nativeSourceCLIClient(cmd)
	if err != nil {
		return err
	}
	workspaceID, _ := cmd.Flags().GetString("workspace")
	sourceID, _ := cmd.Flags().GetString("source")
	source, err := client.GetNativeSourceEnrollment(cmd.Context(), workspaceID, sourceID)
	if err != nil {
		return err
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(source)
}
