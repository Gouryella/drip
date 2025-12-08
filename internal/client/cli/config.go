package cli

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"

	"drip/internal/client/cli/ui"
	"drip/pkg/config"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage configuration",
	Long:  "Manage Drip client configuration (server, token, etc.)",
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize configuration interactively",
	Long:  "Initialize Drip configuration with interactive prompts",
	RunE:  runConfigInit,
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show current configuration",
	Long:  "Display the current Drip configuration",
	RunE:  runConfigShow,
}

var configSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Set configuration values",
	Long:  "Set specific configuration values (server, token)",
	RunE:  runConfigSet,
}

var configResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset configuration",
	Long:  "Delete the configuration file",
	RunE:  runConfigReset,
}

var configValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate configuration",
	Long:  "Validate the configuration file",
	RunE:  runConfigValidate,
}

var (
	configFull   bool
	configForce  bool
	configServer string
	configToken  string
)

func init() {
	configCmd.AddCommand(configInitCmd)
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configResetCmd)
	configCmd.AddCommand(configValidateCmd)

	configShowCmd.Flags().BoolVar(&configFull, "full", false, "Show full token (not hidden)")

	configSetCmd.Flags().StringVar(&configServer, "server", "", "Server address (e.g., tunnel.example.com:443)")
	configSetCmd.Flags().StringVar(&configToken, "token", "", "Authentication token")

	configResetCmd.Flags().BoolVar(&configForce, "force", false, "Force reset without confirmation")

	rootCmd.AddCommand(configCmd)
}

func runConfigInit(cmd *cobra.Command, args []string) error {
	fmt.Print(ui.RenderConfigInit())

	reader := bufio.NewReader(os.Stdin)

	fmt.Print(ui.Muted("Server address (e.g., tunnel.example.com:443): "))
	serverAddr, _ := reader.ReadString('\n')
	serverAddr = strings.TrimSpace(serverAddr)

	if serverAddr == "" {
		return fmt.Errorf("server address is required")
	}

	fmt.Print(ui.Muted("Authentication token (leave empty to skip): "))
	token, _ := reader.ReadString('\n')
	token = strings.TrimSpace(token)

	cfg := &config.ClientConfig{
		Server: serverAddr,
		Token:  token,
		TLS:    true,
	}

	if err := config.SaveClientConfig(cfg, ""); err != nil {
		return fmt.Errorf("failed to save configuration: %w", err)
	}

	fmt.Println(ui.RenderConfigSaved(config.DefaultClientConfigPath()))

	return nil
}

func runConfigShow(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadClientConfig("")
	if err != nil {
		return err
	}

	var displayToken string
	if cfg.Token != "" {
		tokenLen := len(cfg.Token)
		if tokenLen <= 3 {
			// For very short tokens, just show asterisks
			displayToken = "***"
		} else if tokenLen > 10 {
			// For long tokens, show first 3 and last 3 characters
			displayToken = cfg.Token[:3] + "***" + cfg.Token[tokenLen-3:]
		} else {
			// For medium tokens (4-10 chars), show first 3 characters
			displayToken = cfg.Token[:3] + "***"
		}
	} else {
		displayToken = ""
	}

	fmt.Println(ui.RenderConfigShow(cfg.Server, displayToken, !configFull, cfg.TLS, config.DefaultClientConfigPath()))

	return nil
}

func runConfigSet(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadClientConfig("")
	if err != nil {
		cfg = &config.ClientConfig{
			TLS: true,
		}
	}

	modified := false
	var updates []string

	if configServer != "" {
		cfg.Server = configServer
		modified = true
		updates = append(updates, "Server updated: "+configServer)
	}

	if configToken != "" {
		cfg.Token = configToken
		modified = true
		updates = append(updates, "Token updated")
	}

	if !modified {
		return fmt.Errorf("no changes specified. Use --server or --token")
	}

	if err := config.SaveClientConfig(cfg, ""); err != nil {
		return fmt.Errorf("failed to save configuration: %w", err)
	}

	fmt.Println(ui.RenderConfigUpdated(updates))

	return nil
}

func runConfigReset(cmd *cobra.Command, args []string) error {
	configPath := config.DefaultClientConfigPath()

	if !config.ConfigExists("") {
		fmt.Println("No configuration file found")
		return nil
	}

	if !configForce {
		fmt.Print("Are you sure you want to delete the configuration? (y/N): ")
		reader := bufio.NewReader(os.Stdin)
		response, _ := reader.ReadString('\n')
		response = strings.ToLower(strings.TrimSpace(response))

		if response != "y" && response != "yes" {
			fmt.Println("Cancelled")
			return nil
		}
	}

	if err := os.Remove(configPath); err != nil {
		return fmt.Errorf("failed to delete configuration: %w", err)
	}

	fmt.Println(ui.RenderConfigDeleted())

	return nil
}

func runConfigValidate(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadClientConfig("")
	if err != nil {
		fmt.Println(ui.Error("Failed to load configuration"))
		return err
	}

	serverValid, serverMsg := validateServerAddress(cfg.Server)
	tokenSet := cfg.Token != ""
	tlsEnabled := cfg.TLS

	tokenMsg := "Token is set"
	if !tokenSet {
		tokenMsg = "Token is not set (authentication may fail)"
	}

	fmt.Println(ui.RenderConfigValidation(serverValid, serverMsg, tokenSet, tokenMsg, tlsEnabled))

	if !serverValid {
		return fmt.Errorf("invalid configuration: %s", serverMsg)
	}

	return nil
}

func validateServerAddress(addr string) (bool, string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false, "Server address is not set"
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Sprintf("Server address must include host and port (e.g., tunnel.example.com:443): %v", err)
	}

	if host == "" {
		return false, "Server host is empty"
	}

	if port == "" {
		return false, "Server port is empty"
	}

	return true, fmt.Sprintf("Server address is valid (%s:%s)", host, port)
}
