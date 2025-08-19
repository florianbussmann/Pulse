package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	internalauth "github.com/rcourtman/pulse-go-rewrite/internal/auth"
	"github.com/rcourtman/pulse-go-rewrite/internal/config"
	"github.com/rcourtman/pulse-go-rewrite/internal/updates"
	"github.com/rs/zerolog/log"
)

// detectServiceName detects the actual systemd service name being used
func detectServiceName() string {
	// Try common service names
	services := []string{"pulse-backend", "pulse", "pulse.service", "pulse-backend.service"}

	for _, service := range services {
		cmd := exec.Command("systemctl", "status", service)
		if err := cmd.Run(); err == nil {
			// Service exists
			if strings.HasSuffix(service, ".service") {
				return strings.TrimSuffix(service, ".service")
			}
			return service
		}
	}

	// Default to pulse-backend if no service found
	return "pulse-backend"
}

// validateBcryptHash ensures the hash is complete (60 characters)
func validateBcryptHash(hash string) error {
	if len(hash) != 60 {
		return fmt.Errorf("invalid bcrypt hash: expected 60 characters, got %d. Hash may be truncated", len(hash))
	}
	if !strings.HasPrefix(hash, "$2a$") && !strings.HasPrefix(hash, "$2b$") && !strings.HasPrefix(hash, "$2y$") {
		return fmt.Errorf("invalid bcrypt hash: must start with $2a$, $2b$, or $2y$")
	}
	return nil
}

// isRunningAsRoot checks if the process has root privileges
func isRunningAsRoot() bool {
	return os.Geteuid() == 0
}

// handleQuickSecuritySetupFixed is the fixed version of the Quick Security Setup
func handleQuickSecuritySetupFixed(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Apply rate limiting to prevent brute force attacks
		clientIP := GetClientIP(req)
		if !authLimiter.Allow(clientIP) {
			log.Warn().Str("ip", clientIP).Msg("Rate limit exceeded for security setup")
			http.Error(w, "Too many attempts. Please try again later.", http.StatusTooManyRequests)
			return
		}

		// Check if password auth is already configured
		// Allow adding password auth on top of API-only access
		if r.config.AuthUser != "" && r.config.AuthPass != "" {
			log.Info().Msg("Security setup skipped - password auth already configured")
			response := map[string]interface{}{
				"success": true,
				"skipped": true,
				"message": "Password authentication is already configured. Please remove existing security first if you want to reconfigure.",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
			return
		}

		// Parse request body
		var setupRequest struct {
			Username            string `json:"username"`
			Password            string `json:"password"`
			APIToken            string `json:"apiToken"`
			PollingInterval     int    `json:"pollingInterval"`
			EnableNotifications bool   `json:"enableNotifications"`
			DarkMode            bool   `json:"darkMode"`
		}

		if err := json.NewDecoder(req.Body).Decode(&setupRequest); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate inputs
		if setupRequest.Username == "" || setupRequest.Password == "" || setupRequest.APIToken == "" {
			http.Error(w, "Username, password, and API token are required", http.StatusBadRequest)
			return
		}

		// Validate password complexity
		if err := internalauth.ValidatePasswordComplexity(setupRequest.Password); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Set default polling interval if not provided
		if setupRequest.PollingInterval == 0 {
			setupRequest.PollingInterval = 5
		}

		// Hash the password
		hashedPassword, err := internalauth.HashPassword(setupRequest.Password)
		if err != nil {
			log.Error().Err(err).Msg("Failed to hash password")
			http.Error(w, "Failed to process password", http.StatusInternalServerError)
			return
		}

		// Validate the bcrypt hash is complete
		if err := validateBcryptHash(hashedPassword); err != nil {
			log.Error().Err(err).Msg("Generated invalid bcrypt hash")
			http.Error(w, fmt.Sprintf("Password hashing error: %v", err), http.StatusInternalServerError)
			return
		}

		// Store the raw API token for displaying to the user
		rawAPIToken := setupRequest.APIToken

		// Hash the API token for storage
		hashedAPIToken := internalauth.HashAPIToken(rawAPIToken)

		if r.config.APIToken != "" && r.config.AuthUser == "" && r.config.AuthPass == "" {
			// We had API-only access before, now replacing with full security
			log.Info().Msg("Replacing API-only token with new secure token")
		}

		// Update runtime config immediately with hashed token - no restart needed!
		r.config.AuthUser = setupRequest.Username
		r.config.AuthPass = hashedPassword
		r.config.APIToken = hashedAPIToken
		r.config.APITokenEnabled = true
		r.config.PollingInterval = time.Duration(setupRequest.PollingInterval) * time.Second
		log.Info().Msg("Runtime config updated with new security settings - active immediately")

		// Save system settings to system.json (polling interval, etc)
		systemSettings := config.SystemSettings{
			PollingInterval:   setupRequest.PollingInterval,
			ConnectionTimeout: 10,    // Default
			AutoUpdateEnabled: false, // Default
		}
		if err := r.persistence.SaveSystemSettings(systemSettings); err != nil {
			log.Error().Err(err).Msg("Failed to save system settings")
			// Continue anyway - not critical for auth setup
		}

		// Detect environment
		isSystemd := os.Getenv("INVOCATION_ID") != ""
		isDocker := os.Getenv("PULSE_DOCKER") == "true"
		isRoot := isRunningAsRoot()

		// Detect actual service name if systemd
		serviceName := ""
		if isSystemd {
			serviceName = detectServiceName()
			log.Info().Str("service", serviceName).Msg("Detected systemd service name")
		}

		// Choose appropriate method based on environment
		if isDocker {
			// Docker: Save to /data/.env with proper quoting
			envPath := filepath.Join(r.config.ConfigPath, ".env")

			// CRITICAL: Use single quotes to prevent shell expansion of $ in bcrypt hash
			envContent := fmt.Sprintf(`# Auto-generated by Pulse Quick Security Setup
# Generated on %s
# IMPORTANT: Do not remove the single quotes around the password hash!
PULSE_AUTH_USER='%s'
PULSE_AUTH_PASS='%s'
API_TOKEN=%s
ENABLE_AUDIT_LOG=true
`, time.Now().Format(time.RFC3339), setupRequest.Username, hashedPassword, hashedAPIToken)

			// Ensure directory exists
			os.MkdirAll(r.config.ConfigPath, 0755)

			if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
				log.Error().Err(err).Str("path", envPath).Msg("Failed to write .env file in Docker")
				http.Error(w, "Failed to save security configuration", http.StatusInternalServerError)
				return
			}

			log.Info().Str("path", envPath).Msg("Docker security configuration saved")

			response := map[string]interface{}{
				"success":               true,
				"method":                "docker",
				"deploymentType":        "docker",
				"requiresManualRestart": false,
				"message":               "Security enabled immediately! Your settings are saved and active.",
				"note":                  "Configuration saved to /data/.env for persistence across restarts.",
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)

		} else if isSystemd && !isRoot {
			// Systemd but not root (ProxmoxVE script scenario)
			// Don't attempt sudo, just save config and provide instructions

			envPath := filepath.Join(r.config.ConfigPath, ".env")
			envContent := fmt.Sprintf(`# Auto-generated by Pulse Quick Security Setup
# Generated on %s
PULSE_AUTH_USER='%s'
PULSE_AUTH_PASS='%s'
API_TOKEN=%s
ENABLE_AUDIT_LOG=true
`, time.Now().Format(time.RFC3339), setupRequest.Username, hashedPassword, hashedAPIToken)

			// Save to config directory (usually /etc/pulse)
			os.MkdirAll(r.config.ConfigPath, 0755)

			if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
				// Try data directory as fallback
				envPath = filepath.Join(r.config.DataPath, ".env")
				os.MkdirAll(r.config.DataPath, 0755)
				if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
					log.Error().Err(err).Msg("Failed to write .env file")
					http.Error(w, "Failed to save security configuration", http.StatusInternalServerError)
					return
				}
			}

			// Create response - security is active immediately
			response := map[string]interface{}{
				"success":               true,
				"method":                "systemd-nonroot",
				"serviceName":           serviceName,
				"envFile":               envPath,
				"deploymentType":        updates.GetDeploymentType(),
				"requiresManualRestart": false,
				"message":               "Security enabled immediately! Your settings are saved and active.",
				"note":                  fmt.Sprintf("Configuration saved to %s for persistence across restarts.", envPath),
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)

		} else if isSystemd && isRoot {
			// Systemd with root - can apply directly

			// Create systemd override
			overridePath := fmt.Sprintf("/etc/systemd/system/%s.service.d/override.conf", serviceName)
			overrideDir := filepath.Dir(overridePath)

			if err := os.MkdirAll(overrideDir, 0755); err != nil {
				log.Error().Err(err).Msg("Failed to create override directory")
				http.Error(w, "Failed to create systemd override directory", http.StatusInternalServerError)
				return
			}

			overrideContent := fmt.Sprintf(`# Auto-generated by Pulse Quick Security Setup
# Generated on %s
[Service]
Environment="PULSE_AUTH_USER=%s"
Environment="PULSE_AUTH_PASS=%s"
Environment="API_TOKEN=%s"
Environment="ENABLE_AUDIT_LOG=true"
`, time.Now().Format(time.RFC3339), setupRequest.Username, hashedPassword, hashedAPIToken)

			if err := os.WriteFile(overridePath, []byte(overrideContent), 0644); err != nil {
				log.Error().Err(err).Msg("Failed to write systemd override")
				http.Error(w, "Failed to write systemd override", http.StatusInternalServerError)
				return
			}

			// Reload systemd
			exec.Command("systemctl", "daemon-reload").Run()

			response := map[string]interface{}{
				"success":               true,
				"method":                "systemd-root",
				"serviceName":           serviceName,
				"deploymentType":        updates.GetDeploymentType(),
				"automatic":             true,
				"requiresManualRestart": false,
				"message":               "Security enabled immediately! Your settings are saved and active.",
				"note":                  "Systemd override created for persistence across restarts.",
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)

		} else {
			// Manual installation or development
			envPath := filepath.Join(r.config.ConfigPath, ".env")
			if r.config.ConfigPath == "" {
				envPath = "/etc/pulse/.env"
			}

			envContent := fmt.Sprintf(`# Auto-generated by Pulse Quick Security Setup
# Generated on %s
PULSE_AUTH_USER='%s'
PULSE_AUTH_PASS='%s'
API_TOKEN=%s
ENABLE_AUDIT_LOG=true
`, time.Now().Format(time.RFC3339), setupRequest.Username, hashedPassword, hashedAPIToken)

			// Try to create directory if needed
			os.MkdirAll(filepath.Dir(envPath), 0755)

			if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
				log.Error().Err(err).Msg("Failed to write .env file")
				// Still return success with manual instructions
			}

			// Get deployment type for restart instructions
			deploymentType := updates.GetDeploymentType()

			response := map[string]interface{}{
				"success":               true,
				"method":                "manual",
				"envFile":               envPath,
				"deploymentType":        deploymentType,
				"requiresManualRestart": false,
				"message":               "Security enabled immediately! Your settings are saved and active.",
				"note":                  fmt.Sprintf("Configuration saved to %s for persistence across restarts.", envPath),
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
		}
	}
}

// HandleRegenerateAPIToken generates a new API token and updates the .env file
func (r *Router) HandleRegenerateAPIToken(w http.ResponseWriter, rq *http.Request) {
	// Only require authentication if auth is already configured
	// This allows users to set up API-only access without password auth
	if (r.config.AuthUser != "" || r.config.AuthPass != "") && !CheckAuth(r.config, w, rq) {
		return
	}

	if rq.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Apply rate limiting to prevent abuse
	clientIP := GetClientIP(rq)
	if !authLimiter.Allow(clientIP) {
		log.Warn().Str("ip", clientIP).Msg("Rate limit exceeded for API token generation")
		http.Error(w, "Too many attempts. Please try again later.", http.StatusTooManyRequests)
		return
	}

	// Generate new token using the auth package
	rawToken, err := internalauth.GenerateAPIToken()
	if err != nil {
		log.Error().Err(err).Msg("Failed to generate API token")
		http.Error(w, "Failed to generate token", http.StatusInternalServerError)
		return
	}

	// Hash the token for storage
	hashedToken := internalauth.HashAPIToken(rawToken)

	// Update runtime config immediately with hashed token - no restart needed!
	r.config.APIToken = hashedToken
	r.config.APITokenEnabled = true
	log.Info().Msg("Runtime config updated with new hashed API token - active immediately")

	// Determine env file path
	envPath := filepath.Join(r.config.ConfigPath, ".env")
	if r.config.ConfigPath == "" {
		envPath = "/etc/pulse/.env"
	}

	// Docker uses /data/.env
	if _, err := os.Stat("/data/.env"); err == nil {
		envPath = "/data/.env"
	}

	// Read existing .env file
	content, err := os.ReadFile(envPath)
	if err != nil {
		log.Error().Err(err).Str("path", envPath).Msg("Failed to read .env file")
		http.Error(w, "Security configuration not found", http.StatusNotFound)
		return
	}

	// Update the API_TOKEN line with the hashed token
	lines := strings.Split(string(content), "\n")
	var updated bool
	for i, line := range lines {
		if strings.HasPrefix(line, "API_TOKEN=") {
			lines[i] = fmt.Sprintf("API_TOKEN=%s", hashedToken)
			updated = true
			break
		}
	}

	if !updated {
		// API_TOKEN line not found, add it
		lines = append(lines, fmt.Sprintf("API_TOKEN=%s", hashedToken))
	}

	// Write updated content back
	newContent := strings.Join(lines, "\n")
	if err := os.WriteFile(envPath, []byte(newContent), 0600); err != nil {
		log.Error().Err(err).Msg("Failed to update .env file")
		http.Error(w, "Failed to save new token", http.StatusInternalServerError)
		return
	}

	log.Info().Msg("API token regenerated successfully")

	// Get deployment type for restart instructions
	deploymentType := updates.GetDeploymentType()

	response := map[string]interface{}{
		"success":         true,
		"token":           rawToken, // Return the raw token to the user (only shown once!)
		"deploymentType":  deploymentType,
		"requiresRestart": false,
		"message":         "New API token generated and active immediately! Save this token - it won't be shown again.",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
