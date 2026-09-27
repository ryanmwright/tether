package main

import (
	"time"

	"github.com/spf13/cobra"
)

func newGPGCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gpg",
		Short: "Forward gpg-agent to a host",
		Long: "Forward your local gpg-agent to a host, so gpg there signs and decrypts\n" +
			"with your local keys. These are shortcuts for `tether fwd add|rm HOST\n" +
			"gpg-agent [gpg-ssh]`; to forward gpg permanently, set gpg (and gpg_ssh)\n" +
			"in a profile.\n\n" +
			"Run `tether doctor HOST` to check the remote's gpg setup.",
	}

	var ssh, noWait bool
	var timeout time.Duration
	on := &cobra.Command{
		Use:   "on HOST",
		Short: "Forward gpg-agent (and with --ssh, its SSH socket) to HOST",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return addForwards(cmd, g, args[0], gpgForwards(ssh), noWait, timeout)
		},
	}
	on.Flags().BoolVar(&ssh, "ssh", false, "also forward gpg-agent's SSH socket, letting the remote use your SSH keys")
	on.Flags().BoolVar(&noWait, "no-wait", false, "return without waiting for the result")
	on.Flags().DurationVar(&timeout, "timeout", 45*time.Second, "how long to wait")

	var offSSH bool
	off := &cobra.Command{
		Use:   "off HOST",
		Short: "Stop the ad-hoc gpg forwards to HOST",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return removeForwards(cmd, g, args[0], gpgForwards(offSSH))
		},
	}
	off.Flags().BoolVar(&offSSH, "ssh", false, "also stop the SSH socket forward")

	cmd.AddCommand(on, off)
	return cmd
}

func gpgForwards(ssh bool) []string {
	if ssh {
		return []string{"gpg-agent", "gpg-ssh"}
	}
	return []string{"gpg-agent"}
}
