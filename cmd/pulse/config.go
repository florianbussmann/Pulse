package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"syscall"

	"github.com/rcourtman/pulse-go-rewrite/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	exportFile  string
	importFile  string
	passphrase  string
	forceImport bool
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Configuration management commands",
	Long:  `Manage Pulse configuration settings`,
}

var configInfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Show configuration information",
	Long:  `Display information about Pulse configuration`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("Pulse Configuration Information")
		fmt.Println("==============================")
		fmt.Println()
		fmt.Println("Configuration is managed through the web UI.")
		fmt.Println("Settings are stored in encrypted files at /etc/pulse/")
		fmt.Println()
		fmt.Println("Configuration files:")
		fmt.Println("  - nodes.enc      : Encrypted Proxmox node configurations")
		fmt.Println("  - email.enc      : Encrypted email settings")
		fmt.Println("  - system.json    : System settings (polling interval, etc)")
		fmt.Println("  - alerts.json    : Alert rules and thresholds")
		fmt.Println("  - webhooks.json  : Webhook configurations")
		fmt.Println()
		fmt.Println("To configure Pulse, use the Settings tab in the web UI.")
		return nil
	},
}

var configExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export configuration with encryption",
	Long:  `Export all Pulse configuration to an encrypted file`,
	Example: `  # Export with interactive passphrase prompt
  pulse config export -o pulse-config.enc
  
  # Export with passphrase from environment variable
  PULSE_PASSPHRASE=mysecret pulse config export -o pulse-config.enc`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Get passphrase
		pass := getPassphrase("Enter passphrase for encryption: ", false)
		if pass == "" {
			return fmt.Errorf("passphrase is required")
		}

		// Load configuration path
		configPath := os.Getenv("PULSE_DATA_DIR")
		if configPath == "" {
			configPath = "/etc/pulse"
		}

		// Create persistence manager
		persistence := config.NewConfigPersistence(configPath)

		// Export configuration
		exportedData, err := persistence.ExportConfig(pass)
		if err != nil {
			return fmt.Errorf("failed to export configuration: %w", err)
		}

		// Write to file or stdout
		if exportFile != "" {
			if err := ioutil.WriteFile(exportFile, []byte(exportedData), 0600); err != nil {
				return fmt.Errorf("failed to write export file: %w", err)
			}
			fmt.Printf("Configuration exported to %s\n", exportFile)
		} else {
			fmt.Println(exportedData)
		}

		return nil
	},
}

var configImportCmd = &cobra.Command{
	Use:   "import",
	Short: "Import configuration from encrypted export",
	Long:  `Import Pulse configuration from an encrypted export file`,
	Example: `  # Import with interactive passphrase prompt
  pulse config import -i pulse-config.enc
  
  # Import with passphrase from environment variable
  PULSE_PASSPHRASE=mysecret pulse config import -i pulse-config.enc
  
  # Force import without confirmation
  pulse config import -i pulse-config.enc --force`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Check if import file is specified
		if importFile == "" {
			return fmt.Errorf("import file is required (use -i flag)")
		}

		// Read import file
		data, err := ioutil.ReadFile(importFile)
		if err != nil {
			return fmt.Errorf("failed to read import file: %w", err)
		}

		// Get passphrase
		pass := getPassphrase("Enter passphrase for decryption: ", false)
		if pass == "" {
			return fmt.Errorf("passphrase is required")
		}

		// Confirm import unless forced
		if !forceImport {
			fmt.Println("WARNING: This will overwrite all existing configuration!")
			fmt.Print("Continue? (yes/no): ")
			reader := bufio.NewReader(os.Stdin)
			response, _ := reader.ReadString('\n')
			response = strings.TrimSpace(strings.ToLower(response))
			if response != "yes" && response != "y" {
				fmt.Println("Import cancelled")
				return nil
			}
		}

		// Load configuration path
		configPath := os.Getenv("PULSE_DATA_DIR")
		if configPath == "" {
			configPath = "/etc/pulse"
		}

		// Create persistence manager
		persistence := config.NewConfigPersistence(configPath)

		// Import configuration
		if err := persistence.ImportConfig(string(data), pass); err != nil {
			return fmt.Errorf("failed to import configuration: %w", err)
		}

		fmt.Println("Configuration imported successfully")
		fmt.Println("Please restart Pulse for changes to take effect:")
		fmt.Println("  sudo systemctl restart pulse")

		return nil
	},
}

// getPassphrase prompts for a passphrase or gets it from environment
func getPassphrase(prompt string, confirm bool) string {
	// Check environment variable first
	if pass := os.Getenv("PULSE_PASSPHRASE"); pass != "" {
		return pass
	}

	// Check if passphrase flag was set
	if passphrase != "" {
		return passphrase
	}

	// Interactive prompt
	fmt.Print(prompt)
	bytePassword, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		return ""
	}

	pass := string(bytePassword)

	// Confirm if requested
	if confirm {
		fmt.Print("Confirm passphrase: ")
		bytePassword2, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Println()
		if err != nil {
			return ""
		}
		if string(bytePassword2) != pass {
			fmt.Println("Passphrases do not match")
			return ""
		}
	}

	return pass
}

// Environment variable support for initial setup
var configAutoImportCmd = &cobra.Command{
	Use:    "auto-import",
	Hidden: true, // Hidden command for automated setup
	Short:  "Auto-import configuration on startup",
	Long:   `Automatically import configuration from URL or file on first startup`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Check for auto-import environment variables
		configURL := os.Getenv("PULSE_INIT_CONFIG_URL")
		configData := os.Getenv("PULSE_INIT_CONFIG_DATA")
		configPass := os.Getenv("PULSE_INIT_CONFIG_PASSPHRASE")

		if configURL == "" && configData == "" {
			return nil // Nothing to import
		}

		if configPass == "" {
			return fmt.Errorf("PULSE_INIT_CONFIG_PASSPHRASE is required for auto-import")
		}

		var encryptedData string

		// Get data from URL or direct data
		if configURL != "" {
			// TODO: Implement HTTP fetch for config URL
			return fmt.Errorf("URL import not yet implemented")
		} else if configData != "" {
			// Decode base64 if needed
			if decoded, err := base64.StdEncoding.DecodeString(configData); err == nil {
				encryptedData = string(decoded)
			} else {
				encryptedData = configData
			}
		}

		// Load configuration path
		configPath := os.Getenv("PULSE_DATA_DIR")
		if configPath == "" {
			configPath = "/etc/pulse"
		}

		// Create persistence manager
		persistence := config.NewConfigPersistence(configPath)

		// Import configuration
		if err := persistence.ImportConfig(encryptedData, configPass); err != nil {
			return fmt.Errorf("failed to auto-import configuration: %w", err)
		}

		fmt.Println("Configuration auto-imported successfully")
		return nil
	},
}

func init() {
	configCmd.AddCommand(configInfoCmd)
	configCmd.AddCommand(configExportCmd)
	configCmd.AddCommand(configImportCmd)
	configCmd.AddCommand(configAutoImportCmd)

	// Export flags
	configExportCmd.Flags().StringVarP(&exportFile, "output", "o", "", "Output file for encrypted configuration")
	configExportCmd.Flags().StringVarP(&passphrase, "passphrase", "p", "", "Passphrase for encryption (or use PULSE_PASSPHRASE env var)")

	// Import flags
	configImportCmd.Flags().StringVarP(&importFile, "input", "i", "", "Input file with encrypted configuration")
	configImportCmd.Flags().StringVarP(&passphrase, "passphrase", "p", "", "Passphrase for decryption (or use PULSE_PASSPHRASE env var)")
	configImportCmd.Flags().BoolVarP(&forceImport, "force", "f", false, "Force import without confirmation")
}
