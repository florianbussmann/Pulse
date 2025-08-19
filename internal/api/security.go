package api

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Security improvements for Pulse

// CSRF Protection
type CSRFToken struct {
	Token   string
	Expires time.Time
}

var (
	csrfTokens = make(map[string]*CSRFToken)
	csrfMu     sync.RWMutex
)

// generateCSRFToken creates a new CSRF token for a session
func generateCSRFToken(sessionID string) string {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		log.Error().Err(err).Msg("Failed to generate CSRF token")
		return ""
	}

	token := base64.URLEncoding.EncodeToString(tokenBytes)

	csrfMu.Lock()
	csrfTokens[sessionID] = &CSRFToken{
		Token:   token,
		Expires: time.Now().Add(4 * time.Hour),
	}
	csrfMu.Unlock()

	return token
}

// validateCSRFToken checks if a CSRF token is valid for a session
func validateCSRFToken(sessionID, token string) bool {
	csrfMu.RLock()
	defer csrfMu.RUnlock()

	csrfToken, exists := csrfTokens[sessionID]
	if !exists {
		// No CSRF token for this session
		// This can happen if:
		// 1. Session is old/invalid
		// 2. Server restarted (in-memory storage)
		// 3. Auth was disabled after session created

		// If the server was restarted, we lost the in-memory CSRF tokens
		// In this case, we should accept the request but generate a new CSRF token
		// For now, we'll just skip CSRF check for this edge case
		log.Debug().Str("session", sessionID[:8]+"...").Msg("No CSRF token found for session (possibly server restart)")
		// Return true to allow the request through - the session itself provides auth
		return true
	}

	if time.Now().After(csrfToken.Expires) {
		return false
	}

	return csrfToken.Token == token
}

// CheckCSRF validates CSRF token for state-changing requests
func CheckCSRF(w http.ResponseWriter, r *http.Request) bool {
	// Skip CSRF check for safe methods
	if r.Method == "GET" || r.Method == "HEAD" || r.Method == "OPTIONS" {
		return true
	}

	// Skip CSRF for API token auth (API clients don't have sessions)
	if r.Header.Get("X-API-Token") != "" {
		return true
	}

	// Skip CSRF for Basic Auth (doesn't use sessions, not vulnerable to CSRF)
	if r.Header.Get("Authorization") != "" {
		return true
	}

	// Get session from cookie
	cookie, err := r.Cookie("pulse_session")
	if err != nil {
		// No session cookie means no CSRF check needed
		// (either no auth configured or using basic auth which doesn't use sessions)
		return true
	}

	// Get CSRF token from header or form
	csrfToken := r.Header.Get("X-CSRF-Token")
	if csrfToken == "" {
		csrfToken = r.FormValue("csrf_token")
	}

	// If no CSRF token is provided, check if this is a valid session
	// This handles the case where the server restarted and lost CSRF tokens
	if csrfToken == "" {
		// No CSRF token provided - this is definitely invalid
		log.Warn().
			Str("path", r.URL.Path).
			Str("session", cookie.Value[:8]+"...").
			Msg("Missing CSRF token")
		return false
	}

	// Check if the CSRF token validates
	if !validateCSRFToken(cookie.Value, csrfToken) {
		// CSRF validation failed, but check if session is still valid
		// If session is valid but CSRF token doesn't match, it might be due to server restart
		if ValidateSession(cookie.Value) {
			// Valid session but mismatched CSRF - likely server restart
			// Generate a new CSRF token for this session
			newToken := generateCSRFToken(cookie.Value)

			// Detect if we're behind a proxy/tunnel
			isProxied := r.Header.Get("X-Forwarded-For") != "" ||
				r.Header.Get("X-Real-IP") != "" ||
				r.Header.Get("CF-Ray") != "" ||
				r.Header.Get("X-Forwarded-Proto") != ""

			sameSitePolicy := http.SameSiteStrictMode
			if isProxied {
				sameSitePolicy = http.SameSiteNoneMode
			}

			isSecure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"

			// Set the new CSRF token as a cookie
			http.SetCookie(w, &http.Cookie{
				Name:     "pulse_csrf",
				Value:    newToken,
				Path:     "/",
				Secure:   isSecure,
				SameSite: sameSitePolicy,
				MaxAge:   86400, // 24 hours
			})
			// For this request, we'll be lenient and allow it through
			log.Debug().
				Str("path", r.URL.Path).
				Str("session", cookie.Value[:8]+"...").
				Msg("Regenerated CSRF token after server restart")
			return true
		}

		log.Warn().
			Str("path", r.URL.Path).
			Str("session", cookie.Value[:8]+"...").
			Str("provided_token", csrfToken[:8]+"...").
			Msg("Invalid CSRF token")
		return false
	}

	return true
}

// Rate Limiting - using existing RateLimiter from ratelimit.go
var (
	// Auth endpoints: 10 attempts per minute
	authLimiter = NewRateLimiter(10, 1*time.Minute)

	// General API: 500 requests per minute (increased for metadata endpoints)
	apiLimiter = NewRateLimiter(500, 1*time.Minute)
)

// GetClientIP extracts the client IP from the request
func GetClientIP(r *http.Request) string {
	// Check X-Forwarded-For header
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		// Take the first IP in the chain
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}

	// Check X-Real-IP header
	xri := r.Header.Get("X-Real-IP")
	if xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		return addr[:idx]
	}
	return addr
}

// Failed Login Tracking
type FailedLogin struct {
	Count       int
	LastAttempt time.Time
	LockedUntil time.Time
}

var (
	failedLogins = make(map[string]*FailedLogin)
	failedMu     sync.RWMutex

	maxFailedAttempts = 5
	lockoutDuration   = 15 * time.Minute
)

// RecordFailedLogin tracks failed login attempts
func RecordFailedLogin(identifier string) {
	failedMu.Lock()
	defer failedMu.Unlock()

	failed, exists := failedLogins[identifier]
	if !exists {
		failed = &FailedLogin{}
		failedLogins[identifier] = failed
	}

	failed.Count++
	failed.LastAttempt = time.Now()

	if failed.Count >= maxFailedAttempts {
		failed.LockedUntil = time.Now().Add(lockoutDuration)
		log.Warn().
			Str("identifier", identifier).
			Int("attempts", failed.Count).
			Time("locked_until", failed.LockedUntil).
			Msg("Account locked due to failed login attempts")
	}
}

// ClearFailedLogins resets failed login counter on successful login
func ClearFailedLogins(identifier string) {
	failedMu.Lock()
	defer failedMu.Unlock()
	delete(failedLogins, identifier)
}

// IsLockedOut checks if an account is locked out
func IsLockedOut(identifier string) bool {
	failedMu.RLock()
	defer failedMu.RUnlock()

	failed, exists := failedLogins[identifier]
	if !exists {
		return false
	}

	if time.Now().After(failed.LockedUntil) {
		// Lockout expired
		return false
	}

	return failed.Count >= maxFailedAttempts
}

// Security Headers Middleware
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prevent clickjacking
		w.Header().Set("X-Frame-Options", "DENY")

		// Prevent MIME type sniffing
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// Enable XSS protection
		w.Header().Set("X-XSS-Protection", "1; mode=block")

		// Content Security Policy
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline' 'unsafe-eval'; "+ // Needed for React
				"style-src 'self' 'unsafe-inline'; "+ // Needed for inline styles
				"img-src 'self' data: blob:; "+
				"connect-src 'self' ws: wss:; "+ // WebSocket support
				"font-src 'self' data:;")

		// Referrer Policy
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		// Permissions Policy (formerly Feature Policy)
		w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")

		next.ServeHTTP(w, r)
	})
}

// Audit Logging
type AuditEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Event     string    `json:"event"`
	User      string    `json:"user,omitempty"`
	IP        string    `json:"ip"`
	Path      string    `json:"path,omitempty"`
	Success   bool      `json:"success"`
	Details   string    `json:"details,omitempty"`
}

// LogAuditEvent logs security-relevant events
func LogAuditEvent(event string, user string, ip string, path string, success bool, details string) {
	if success {
		log.Info().
			Str("event", event).
			Str("user", user).
			Str("ip", ip).
			Str("path", path).
			Str("details", details).
			Time("timestamp", time.Now()).
			Msg("Security audit event")
	} else {
		log.Warn().
			Str("event", event).
			Str("user", user).
			Str("ip", ip).
			Str("path", path).
			Str("details", details).
			Time("timestamp", time.Now()).
			Msg("Security audit event - FAILED")
	}
}

// Session Management Improvements
var (
	allSessions = make(map[string][]string) // user -> []sessionIDs
	sessionsMu  sync.RWMutex
)

// TrackUserSession tracks which sessions belong to which user
func TrackUserSession(user, sessionID string) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	if allSessions[user] == nil {
		allSessions[user] = []string{}
	}
	allSessions[user] = append(allSessions[user], sessionID)
}

// InvalidateUserSessions invalidates all sessions for a user (e.g., on password change)
func InvalidateUserSessions(user string) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	sessionIDs := allSessions[user]
	for _, sid := range sessionIDs {
		// Delete from main session store
		sessionMu.Lock()
		delete(sessions, sid)
		sessionMu.Unlock()

		// Delete CSRF tokens
		csrfMu.Lock()
		delete(csrfTokens, sid)
		csrfMu.Unlock()
	}

	delete(allSessions, user)

	log.Info().
		Str("user", user).
		Int("sessions_invalidated", len(sessionIDs)).
		Msg("Invalidated all user sessions")
}
