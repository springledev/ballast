package main

import (
	"fmt"
	"os"

	"github.com/springledev/ballast/internal/api"
	"github.com/spf13/cobra"
	"github.com/springledev/keel/dockerhost"
)

var (
	hostKind     string
	hostAddress  string
	hostSSHKey   string
	hostTLSCA    string
	hostTLSCert  string
	hostTLSKey   string
	hostSkipTest bool
)

// readHostCredentials builds a dockerhost.Credentials from the
// command's credential file flags. Only a file path is ever accepted
// as a flag value; the file's contents are read directly (unlike a
// destination's secret access key, a Docker host's SSH key or TLS
// certificates are not the kind of one-line secret worth typing at a
// prompt, and they're already sitting in files most users generated
// with ssh-keygen or their CA tooling).
func readHostCredentials() (dockerhost.Credentials, error) {
	hasTLS := hostTLSCA != "" || hostTLSCert != "" || hostTLSKey != ""
	if hostSSHKey != "" && hasTLS {
		return dockerhost.Credentials{}, fmt.Errorf("give either --ssh-key-file or the --tls-ca/--tls-cert/--tls-key flags, not both")
	}
	if hostSSHKey != "" {
		data, err := os.ReadFile(hostSSHKey)
		if err != nil {
			return dockerhost.Credentials{}, fmt.Errorf("reading SSH key file %s: %w", hostSSHKey, err)
		}
		return dockerhost.Credentials{SSHPrivateKey: string(data)}, nil
	}
	if hasTLS {
		if hostTLSCA == "" || hostTLSCert == "" || hostTLSKey == "" {
			return dockerhost.Credentials{}, fmt.Errorf("a TLS host needs all three of --tls-ca, --tls-cert and --tls-key")
		}
		ca, err := os.ReadFile(hostTLSCA)
		if err != nil {
			return dockerhost.Credentials{}, fmt.Errorf("reading CA certificate file %s: %w", hostTLSCA, err)
		}
		cert, err := os.ReadFile(hostTLSCert)
		if err != nil {
			return dockerhost.Credentials{}, fmt.Errorf("reading client certificate file %s: %w", hostTLSCert, err)
		}
		key, err := os.ReadFile(hostTLSKey)
		if err != nil {
			return dockerhost.Credentials{}, fmt.Errorf("reading client key file %s: %w", hostTLSKey, err)
		}
		return dockerhost.Credentials{CACert: string(ca), ClientCert: string(cert), ClientKey: string(key)}, nil
	}
	return dockerhost.Credentials{}, nil
}

var hostAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Configure a new Docker host",
	Long: `Configure a new Docker host: the machine a service's containers run on.

--kind chooses how Ballast reaches it. Examples:

  # A daemon on another machine, reached over SSH (preferred: no certificates)
  ballast host add rpi --kind remote-ssh --address ballast@192.168.1.20 --ssh-key-file ./ballast_host_key

  # A daemon listening on TCP with TLS client certificates
  ballast host add nas --kind remote-tls --address 192.168.1.30 \
      --tls-ca ca.pem --tls-cert cert.pem --tls-key key.pem

By default Ballast connects with the new settings before saving, so a typo
never leaves a host that can't be reached; pass --no-test to save a host
that is offline right now.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		creds, err := readHostCredentials()
		if err != nil {
			return err
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			h := dockerhost.Host{Name: args[0], Kind: dockerhost.Kind(hostKind), Address: hostAddress}
			req := api.DockerHostSetup{Host: h, Credentials: creds, SkipTest: hostSkipTest}
			res, err := a.ops.SetupDockerHost(cmd.Context(), req)
			if err != nil {
				return err
			}
			cmd.Printf("%s: configured (%s)", res.Host.Name, res.Host.Kind)
			if res.Host.Address != "" {
				cmd.Printf(" at %s", res.Host.Address)
			}
			cmd.Println()
			if res.Test != nil {
				cmd.Printf("  connected — Docker %s\n", res.Test.ServerVersion)
			} else {
				cmd.Println("  not tested (--no-test): the host was saved without connecting to it")
			}
			return nil
		})
	},
}

var hostListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured Docker hosts",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			list, err := a.ops.ListDockerHosts(cmd.Context())
			if err != nil {
				return err
			}
			if len(list) == 0 {
				cmd.Println("No Docker hosts configured yet.")
				return nil
			}
			for _, h := range list {
				cmd.Printf("%s\t%s\t%s\n", h.Name, h.Kind, h.Address)
			}
			return nil
		})
	},
}

var hostTestCmd = &cobra.Command{
	Use:   "test <name>",
	Short: "Check that a Docker host is reachable",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			tr, err := a.ops.TestDockerHost(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			cmd.Printf("%s: reachable — Docker %s\n", args[0], tr.ServerVersion)
			return nil
		})
	},
}

var hostRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a configured Docker host",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			if err := a.ops.DeleteDockerHost(cmd.Context(), args[0]); err != nil {
				return err
			}
			cmd.Printf("%s: removed\n", args[0])
			return nil
		})
	},
}

// registerHostCommands attaches `ballast host ...` (spec §14 step 18,
// §17.2) to root.
func registerHostCommands(root *cobra.Command) {
	hostAddCmd.Flags().StringVar(&hostKind, "kind", "", "how Ballast reaches this Docker daemon: local, remote-ssh, or remote-tls (required)")
	hostAddCmd.Flags().StringVar(&hostAddress, "address", "", "where the daemon is, e.g. user@host (remote-ssh) or host:port (remote-tls)")
	hostAddCmd.Flags().StringVar(&hostSSHKey, "ssh-key-file", "", "path to an unencrypted SSH private key file (remote-ssh only; omit to use the Ballast service user's own SSH keys/agent)")
	hostAddCmd.Flags().StringVar(&hostTLSCA, "tls-ca", "", "path to the CA certificate file (remote-tls only)")
	hostAddCmd.Flags().StringVar(&hostTLSCert, "tls-cert", "", "path to the client certificate file (remote-tls only)")
	hostAddCmd.Flags().StringVar(&hostTLSKey, "tls-key", "", "path to the client key file (remote-tls only)")
	hostAddCmd.Flags().BoolVar(&hostSkipTest, "no-test", false, "save the host without connecting to it first")
	_ = hostAddCmd.MarkFlagRequired("kind")

	hostCmd := &cobra.Command{Use: "host", Short: "Manage the Docker hosts services run on"}
	hostCmd.AddCommand(hostAddCmd, hostListCmd, hostTestCmd, hostRemoveCmd)
	root.AddCommand(hostCmd)
}
