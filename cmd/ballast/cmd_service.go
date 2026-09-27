package main

import (
	"fmt"
	"strings"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/spf13/cobra"
)

var (
	serviceCatalogEntry string
	serviceDestination  string
	serviceDockerHost   string
	serviceParams       []string
	serviceContainers   []string
	serviceSecrets      []string
)

// parseKV parses a list of "key=value" flag values into a map.
func parseKV(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid key=value pair %q (want e.g. path=/data)", p)
		}
		out[k] = v
	}
	return out, nil
}

var serviceAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Configure a new backup service from the catalog",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		params, err := parseKV(serviceParams)
		if err != nil {
			return err
		}
		containers, err := parseKV(serviceContainers)
		if err != nil {
			return err
		}
		secrets, err := parseKV(serviceSecrets)
		if err != nil {
			return err
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			s := inventory.Service{
				Name: args[0], CatalogEntry: serviceCatalogEntry, Destination: serviceDestination, DockerHost: serviceDockerHost,
				Params: params, Containers: containers, Secrets: secrets,
			}
			if err := a.ops.PutService(cmd.Context(), s); err != nil {
				return err
			}
			cmd.Printf("%s: configured (catalog entry %s, destination %s)\n", args[0], serviceCatalogEntry, serviceDestination)
			if serviceDockerHost != "" {
				cmd.Printf("  Docker host: %s\n", serviceDockerHost)
			}
			return nil
		})
	},
}

var serviceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured backup services",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			list, err := a.ops.ListServices(cmd.Context())
			if err != nil {
				return err
			}
			if len(list) == 0 {
				cmd.Println("No services configured yet. Run `ballast service add` to add one.")
				return nil
			}
			for _, s := range list {
				cmd.Printf("%s\tcatalog=%s\tdestination=%s\thost=%s\n", s.Name, s.CatalogEntry, s.Destination, s.DockerHost)
			}
			return nil
		})
	},
}

var serviceRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a configured backup service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return withApp(cfg, cmd.ErrOrStderr(), func(a *app) error {
			if err := a.ops.DeleteService(cmd.Context(), args[0]); err != nil {
				return err
			}
			cmd.Printf("%s: removed\n", args[0])
			return nil
		})
	},
}

// registerServiceCommands attaches `ballast service ...` (spec §9:
// "ballast destination add | test <name>" and the equivalent service
// surface) to root.
func registerServiceCommands(root *cobra.Command) {
	serviceAddCmd.Flags().StringVar(&serviceCatalogEntry, "catalog", "", "catalog entry to back this service with (required)")
	serviceAddCmd.Flags().StringVar(&serviceDestination, "destination", "", "destination to back up to (required)")
	serviceAddCmd.Flags().StringVar(&serviceDockerHost, "host", "", "Docker host this service's containers run on (default: local)")
	serviceAddCmd.Flags().StringArrayVar(&serviceParams, "param", nil, "catalog parameter as key=value (repeatable)")
	serviceAddCmd.Flags().StringArrayVar(&serviceContainers, "container", nil, "detected container as key=value (repeatable)")
	serviceAddCmd.Flags().StringArrayVar(&serviceSecrets, "secret", nil,
		"catalog {{ .Secrets.NAME }} reference as name=credential-file-name (repeatable); leave the value empty (name=) to use the default file name, then set it with `ballast secret set`")
	_ = serviceAddCmd.MarkFlagRequired("catalog")
	_ = serviceAddCmd.MarkFlagRequired("destination")

	serviceCmd := &cobra.Command{Use: "service", Short: "Manage configured backup services"}
	serviceCmd.AddCommand(serviceAddCmd, serviceListCmd, serviceRemoveCmd)
	root.AddCommand(serviceCmd)
}
