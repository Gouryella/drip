package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"drip/internal/server/auth"
	"drip/internal/server/store"
	"drip/internal/shared/ui"
	"drip/pkg/config"

	"github.com/spf13/cobra"
)

var adminDBPath string

var adminCmd = &cobra.Command{
	Use:           "admin",
	Short:         "Manage the Drip control plane",
	Long:          `Manage accounts, client credentials and admin users in the Drip control plane database.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.AddCommand(adminCmd)
	adminCmd.PersistentFlags().StringVar(&adminDBPath, "db", getEnvString("DRIP_DB_PATH", ""),
		"Path to the control plane database (env: DRIP_DB_PATH; falls back to db_path in the server config)")

	adminCmd.AddCommand(adminAccountCmd)
	adminAccountCmd.AddCommand(adminAccountCreateCmd, adminAccountListCmd)
	adminAccountCreateCmd.Flags().Int("max-tunnels", 0, "Maximum concurrent tunnels for this account (0 = unlimited)")

	adminCmd.AddCommand(adminClientCmd)
	adminClientCmd.AddCommand(
		adminClientCreateCmd,
		adminClientListCmd,
		adminClientRotateCmd,
		adminClientEnableCmd,
		adminClientDisableCmd,
		adminClientDeleteCmd,
	)
	adminClientCreateCmd.Flags().String("account", "", "Account name that owns the credential (required)")
	adminClientCreateCmd.Flags().String("bandwidth", "", "Per-client bandwidth limit, e.g. 1M (optional)")
	adminClientListCmd.Flags().String("account", "", "Filter by account name")

	adminCmd.AddCommand(adminReservationCmd)
	adminReservationCmd.AddCommand(
		adminReservationCreateCmd,
		adminReservationListCmd,
		adminReservationBindCmd,
		adminReservationEnableCmd,
		adminReservationDisableCmd,
		adminReservationDeleteCmd,
	)
	adminReservationCreateCmd.Flags().String("account", "", "Account that owns the reservation (required)")
	adminReservationCreateCmd.Flags().String("client", "", "Bind to this client credential ID; unbound reservations may be claimed by any client on the account that asks for them by name")
	adminReservationCreateCmd.Flags().String("subdomain", "", "Subdomain to reserve, for http/https tunnels")
	adminReservationCreateCmd.Flags().Int("tcp-port", 0, "TCP port to reserve, for tcp tunnels")
	adminReservationCreateCmd.Flags().String("bandwidth", "", "Per-tunnel bandwidth limit, e.g. 1M (optional)")
	adminReservationListCmd.Flags().String("account", "", "Filter by account name")
	adminReservationBindCmd.Flags().String("client", "", "Client credential ID to bind to; empty unbinds")
}

var adminReservationCmd = &cobra.Command{
	Use:     "reservation",
	Aliases: []string{"reservations"},
	Short:   "Manage tunnel reservations",
}

var adminReservationCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Reserve a subdomain or a TCP port",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		accountName, _ := cmd.Flags().GetString("account")
		if strings.TrimSpace(accountName) == "" {
			return fmt.Errorf("--account is required")
		}
		subdomainFlag, _ := cmd.Flags().GetString("subdomain")
		tcpPort, _ := cmd.Flags().GetInt("tcp-port")
		clientID, _ := cmd.Flags().GetString("client")
		bandwidth, _ := cmd.Flags().GetString("bandwidth")

		if (subdomainFlag == "") == (tcpPort == 0) {
			return fmt.Errorf("pass exactly one of --subdomain or --tcp-port")
		}

		return withStore(func(ctx context.Context, s *store.Store) error {
			acct, err := s.GetAccountByName(ctx, accountName)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("account %q not found", accountName)
				}
				return err
			}

			reservation := &store.Reservation{
				AccountID: acct.ID,
				Subdomain: subdomainFlag,
				TCPPort:   tcpPort,
				Bandwidth: bandwidth,
				Enabled:   true,
			}
			if subdomainFlag != "" {
				reservation.TunnelType = store.TunnelTypeHTTP
			} else {
				reservation.TunnelType = store.TunnelTypeTCP
			}

			if clientID != "" {
				client, cerr := s.GetClient(ctx, clientID)
				if cerr != nil {
					if errors.Is(cerr, store.ErrNotFound) {
						return fmt.Errorf("client %q not found", clientID)
					}
					return cerr
				}
				if client.AccountID != acct.ID {
					return fmt.Errorf("client %s belongs to a different account", clientID)
				}
				reservation.ClientID = &clientID
			}

			if err := s.CreateReservation(ctx, reservation); err != nil {
				return err
			}

			fmt.Printf("Reservation created\n  id:       %s\n  account:  %s\n  target:   %s\n  client:   %s\n",
				reservation.ID, acct.Name, reservation.Target(), formatClientBinding(reservation.ClientID))
			return nil
		})
	},
}

var adminReservationListCmd = &cobra.Command{
	Use:   "list",
	Short: "List reservations",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		accountName, _ := cmd.Flags().GetString("account")

		return withStore(func(ctx context.Context, s *store.Store) error {
			accountID := ""
			if accountName != "" {
				acct, err := s.GetAccountByName(ctx, accountName)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return fmt.Errorf("account %q not found", accountName)
					}
					return err
				}
				accountID = acct.ID
			}

			list, err := s.ListReservations(ctx, accountID)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Println("No reservations yet. Create one with: drip admin reservation create --account <account> --subdomain <name>")
				return nil
			}

			table := ui.NewTable([]string{"ID", "TYPE", "TARGET", "CLIENT", "ENABLED", "BANDWIDTH"})
			for _, r := range list {
				bw := r.Bandwidth
				if bw == "" {
					bw = "-"
				}
				table.AddRow([]string{
					r.ID, r.TunnelType, r.Target(),
					formatClientBinding(r.ClientID), formatBool(r.Enabled), bw,
				})
			}
			fmt.Println(table.Render())
			return nil
		})
	},
}

var adminReservationBindCmd = &cobra.Command{
	Use:   "bind <reservation-id>",
	Short: "Bind a reservation to a client credential, or unbind it",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		clientID, _ := cmd.Flags().GetString("client")

		return withStore(func(ctx context.Context, s *store.Store) error {
			reservation, err := s.GetReservation(ctx, args[0])
			if err != nil {
				return err
			}

			if clientID == "" {
				reservation.ClientID = nil
			} else {
				client, cerr := s.GetClient(ctx, clientID)
				if cerr != nil {
					if errors.Is(cerr, store.ErrNotFound) {
						return fmt.Errorf("client %q not found", clientID)
					}
					return cerr
				}
				if client.AccountID != reservation.AccountID {
					return fmt.Errorf("client %s belongs to a different account", clientID)
				}
				reservation.ClientID = &clientID
			}

			if err := s.UpdateReservation(ctx, reservation); err != nil {
				return err
			}
			fmt.Printf("Reservation %s (%s) is now bound to %s\n",
				reservation.ID, reservation.Target(), formatClientBinding(reservation.ClientID))
			return nil
		})
	},
}

var adminReservationEnableCmd = &cobra.Command{
	Use:   "enable <reservation-id>",
	Short: "Enable a reservation",
	Args:  cobra.ExactArgs(1),
	RunE:  func(_ *cobra.Command, args []string) error { return setReservationEnabled(args[0], true) },
}

var adminReservationDisableCmd = &cobra.Command{
	Use:   "disable <reservation-id>",
	Short: "Disable a reservation without releasing the name",
	Args:  cobra.ExactArgs(1),
	RunE:  func(_ *cobra.Command, args []string) error { return setReservationEnabled(args[0], false) },
}

func setReservationEnabled(id string, enabled bool) error {
	return withStore(func(ctx context.Context, s *store.Store) error {
		reservation, err := s.GetReservation(ctx, id)
		if err != nil {
			return err
		}
		reservation.Enabled = enabled
		if err := s.UpdateReservation(ctx, reservation); err != nil {
			return err
		}
		fmt.Printf("Reservation %s (%s) is now %s\n", reservation.ID, reservation.Target(), formatBool(enabled))
		return nil
	})
}

var adminReservationDeleteCmd = &cobra.Command{
	Use:   "delete <reservation-id>",
	Short: "Delete a reservation, releasing the name",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return withStore(func(ctx context.Context, s *store.Store) error {
			if err := s.DeleteReservation(ctx, args[0]); err != nil {
				return err
			}
			fmt.Printf("Reservation %s deleted\n", args[0])
			return nil
		})
	},
}

func formatClientBinding(clientID *string) string {
	if clientID == nil || *clientID == "" {
		return "any client on the account"
	}
	return *clientID
}

// resolveDBPath figures out which database to operate on: the --db flag, the
// DRIP_DB_PATH environment variable, or db_path from the server config file.
func resolveDBPath() (string, error) {
	if adminDBPath != "" {
		return adminDBPath, nil
	}

	if config.ServerConfigExists("") {
		cfg, err := config.LoadServerConfig("")
		if err == nil && cfg.DBPath != "" {
			return cfg.DBPath, nil
		}
	}

	return "", fmt.Errorf("no control plane database configured: pass --db, set DRIP_DB_PATH, or set db_path in the server config")
}

// withStore opens the control plane database for the duration of fn.
func withStore(fn func(ctx context.Context, s *store.Store) error) error {
	path, err := resolveDBPath()
	if err != nil {
		return err
	}

	s, err := store.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return fn(ctx, s)
}

var adminAccountCmd = &cobra.Command{
	Use:   "account",
	Short: "Manage accounts",
}

var adminAccountCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create an account",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		maxTunnels, _ := cmd.Flags().GetInt("max-tunnels")

		return withStore(func(ctx context.Context, s *store.Store) error {
			acct, err := s.CreateAccount(ctx, args[0], maxTunnels)
			if err != nil {
				return err
			}
			fmt.Printf("Account created\n  id:           %s\n  name:         %s\n  max tunnels:  %s\n",
				acct.ID, acct.Name, formatLimit(acct.MaxTunnels))
			return nil
		})
	},
}

var adminAccountListCmd = &cobra.Command{
	Use:   "list",
	Short: "List accounts",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return withStore(func(ctx context.Context, s *store.Store) error {
			accounts, err := s.ListAccounts(ctx)
			if err != nil {
				return err
			}
			if len(accounts) == 0 {
				fmt.Println("No accounts yet. Create one with: drip admin account create <name>")
				return nil
			}

			table := ui.NewTable([]string{"ID", "NAME", "ENABLED", "MAX TUNNELS", "CREATED"})
			for _, a := range accounts {
				table.AddRow([]string{
					a.ID,
					a.Name,
					formatBool(a.Enabled),
					formatLimit(a.MaxTunnels),
					a.CreatedAt.Format(time.RFC3339),
				})
			}
			fmt.Println(table.Render())
			return nil
		})
	},
}

var adminClientCmd = &cobra.Command{
	Use:   "client",
	Short: "Manage client credentials",
}

var adminClientCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a client credential",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		accountName, _ := cmd.Flags().GetString("account")
		if strings.TrimSpace(accountName) == "" {
			return fmt.Errorf("--account is required")
		}
		bandwidth, _ := cmd.Flags().GetString("bandwidth")

		return withStore(func(ctx context.Context, s *store.Store) error {
			acct, err := s.GetAccountByName(ctx, accountName)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("account %q not found", accountName)
				}
				return err
			}

			cred, err := auth.GenerateCredential()
			if err != nil {
				return err
			}

			client := &store.Client{
				ID:         cred.ID,
				AccountID:  acct.ID,
				Name:       args[0],
				SecretHash: auth.HashSecret(cred.Secret),
				Enabled:    true,
				Bandwidth:  bandwidth,
			}
			if err := s.CreateClient(ctx, client); err != nil {
				return err
			}

			fmt.Printf("Client credential created\n  account:  %s\n  name:     %s\n  id:       %s\n\n",
				acct.Name, client.Name, client.ID)
			fmt.Printf("  token:    %s\n\n", cred.String())
			fmt.Fprintln(os.Stderr, "This token is shown once and cannot be recovered. Store it now.")
			return nil
		})
	},
}

var adminClientListCmd = &cobra.Command{
	Use:   "list",
	Short: "List client credentials",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		accountName, _ := cmd.Flags().GetString("account")

		return withStore(func(ctx context.Context, s *store.Store) error {
			accountID := ""
			if accountName != "" {
				acct, err := s.GetAccountByName(ctx, accountName)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return fmt.Errorf("account %q not found", accountName)
					}
					return err
				}
				accountID = acct.ID
			}

			clients, err := s.ListClients(ctx, accountID)
			if err != nil {
				return err
			}
			if len(clients) == 0 {
				fmt.Println("No client credentials yet. Create one with: drip admin client create --account <account> <name>")
				return nil
			}

			table := ui.NewTable([]string{"ID", "NAME", "ENABLED", "BANDWIDTH", "LAST SEEN"})
			for _, c := range clients {
				lastSeen := "never"
				if c.LastSeenAt != nil {
					lastSeen = c.LastSeenAt.Format(time.RFC3339)
				}
				bw := c.Bandwidth
				if bw == "" {
					bw = "-"
				}
				table.AddRow([]string{c.ID, c.Name, formatBool(c.Enabled), bw, lastSeen})
			}
			fmt.Println(table.Render())
			return nil
		})
	},
}

var adminClientRotateCmd = &cobra.Command{
	Use:   "rotate <client-id>",
	Short: "Issue a new secret for a client, invalidating the old one",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return withStore(func(ctx context.Context, s *store.Store) error {
			client, err := s.GetClient(ctx, args[0])
			if err != nil {
				return err
			}

			cred, err := auth.GenerateCredential()
			if err != nil {
				return err
			}
			// The credential ID is the primary key, so rotation keeps it and
			// replaces only the secret half.
			cred.ID = client.ID

			if err := s.RotateClientSecret(ctx, client.ID, auth.HashSecret(cred.Secret)); err != nil {
				return err
			}

			fmt.Printf("Secret rotated for client %s (%s)\n\n  token:    %s\n\n",
				client.Name, client.ID, cred.String())
			fmt.Fprintln(os.Stderr, "The previous token stops working within the credential cache TTL (30s).")
			return nil
		})
	},
}

var adminClientEnableCmd = &cobra.Command{
	Use:   "enable <client-id>",
	Short: "Enable a client credential",
	Args:  cobra.ExactArgs(1),
	RunE:  func(_ *cobra.Command, args []string) error { return setClientEnabled(args[0], true) },
}

var adminClientDisableCmd = &cobra.Command{
	Use:   "disable <client-id>",
	Short: "Disable a client credential",
	Args:  cobra.ExactArgs(1),
	RunE:  func(_ *cobra.Command, args []string) error { return setClientEnabled(args[0], false) },
}

func setClientEnabled(id string, enabled bool) error {
	return withStore(func(ctx context.Context, s *store.Store) error {
		client, err := s.GetClient(ctx, id)
		if err != nil {
			return err
		}
		client.Enabled = enabled
		if err := s.UpdateClient(ctx, client); err != nil {
			return err
		}
		fmt.Printf("Client %s (%s) is now %s\n", client.Name, client.ID, formatBool(enabled))
		return nil
	})
}

var adminClientDeleteCmd = &cobra.Command{
	Use:   "delete <client-id>",
	Short: "Delete a client credential",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return withStore(func(ctx context.Context, s *store.Store) error {
			if err := s.DeleteClient(ctx, args[0]); err != nil {
				return err
			}
			fmt.Printf("Client %s deleted\n", args[0])
			return nil
		})
	},
}

func formatBool(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func formatLimit(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d", n)
}
