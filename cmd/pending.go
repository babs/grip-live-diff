package cmd

import (
	"fmt"
	"os"

	"github.com/babs/grip-live-diff/internal"
	"github.com/spf13/cobra"
)

var pendingCmd = &cobra.Command{
	Use:   "pending --port <port> --pid <pid> [--claim <agent pid>] <path>...",
	Short: "List the open annotation entries the agent of an instance must handle",
	Long: `List the open annotation entries the agent of an instance must handle, one
"<sidecar>\t<entry id>" line each; exit 1 when there is none, 2 when the instance is gone or a
path or an argument is wrong.

<path> is a sidecar, a markdown file, a ready file or a directory (its sidecars, recursively); a
sidecar that cannot be read is reported and skipped. The instance is the port and PID of its start
line. In manual mode, a "<sidecar>\tready" line comes first when the batch is the instance's and
unclaimed; --claim <agent pid> takes it, under the sidecar flock, and takes exactly one sidecar.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		port, _ := cmd.Flags().GetInt("port")
		pid, _ := cmd.Flags().GetInt64("pid")
		claim, _ := cmd.Flags().GetInt64("claim")
		if port < 1 || port > 65535 {
			// Port 0 reads as "no port" in the owner rule, which routing off would turn into all.
			return fmt.Errorf("--port %d: not a port", port)
		}
		if cmd.Flags().Changed("claim") && claim <= 0 {
			// 0 is "no claim" inside: a claim that takes nothing must not exit 0.
			return fmt.Errorf("--claim %d: not a PID", claim)
		}
		found, err := internal.Pending{
			Port:     port,
			PID:      pid,
			Claim:    claim,
			Registry: internal.DefaultRegistryDir(),
			Paths:    args,
			Out:      os.Stdout,
			Err:      os.Stderr,
		}.Run()
		if err != nil {
			// 1 is "nothing to do": a Monitor looping on it must stop on an error.
			_, _ = fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(2)
		}
		if !found {
			os.Exit(1)
		}
		return nil
	},
}

func init() {
	pendingCmd.Flags().Int("port", 0, "Port of the instance, from its start line")
	pendingCmd.Flags().Int64("pid", 0, "PID of the instance, from its start line")
	pendingCmd.Flags().Int64("claim", 0, "Claim the ready file with this agent PID")
	_ = pendingCmd.MarkFlagRequired("port")
	_ = pendingCmd.MarkFlagRequired("pid")
	rootCmd.AddCommand(pendingCmd)
}
