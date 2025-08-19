package notifications

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// EnhancedEmailManager extends email functionality with provider support
type EnhancedEmailManager struct {
	config    EmailProviderConfig
	rateLimit *RateLimiter
}

// RateLimiter implements a simple rate limiter
type RateLimiter struct {
	rate      int
	lastSent  time.Time
	sentCount int
}

// NewEnhancedEmailManager creates an enhanced email manager
func NewEnhancedEmailManager(config EmailProviderConfig) *EnhancedEmailManager {
	return &EnhancedEmailManager{
		config: config,
		rateLimit: &RateLimiter{
			rate: config.RateLimit,
		},
	}
}

// SendEmailWithRetry sends email with retry logic
func (e *EnhancedEmailManager) SendEmailWithRetry(subject, htmlBody, textBody string) error {
	var lastErr error

	for attempt := 0; attempt <= e.config.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(e.config.RetryDelay) * time.Second
			log.Debug().
				Int("attempt", attempt).
				Dur("delay", delay).
				Msg("Retrying email send after delay")
			time.Sleep(delay)
		}

		// Check rate limit
		if err := e.checkRateLimit(); err != nil {
			lastErr = err
			continue
		}

		// Try to send
		err := e.sendEmailOnce(subject, htmlBody, textBody)
		if err == nil {
			if attempt > 0 {
				log.Info().
					Int("attempt", attempt).
					Msg("Email sent successfully after retry")
			}
			return nil
		}

		lastErr = err
		log.Warn().
			Err(err).
			Int("attempt", attempt).
			Str("provider", e.config.Provider).
			Msg("Email send attempt failed")
	}

	return fmt.Errorf("email failed after %d attempts: %w", e.config.MaxRetries+1, lastErr)
}

// checkRateLimit enforces rate limiting
func (e *EnhancedEmailManager) checkRateLimit() error {
	if e.config.RateLimit <= 0 {
		return nil // No rate limit
	}

	now := time.Now()
	if now.Sub(e.rateLimit.lastSent) >= time.Minute {
		// Reset counter after a minute
		e.rateLimit.sentCount = 0
		e.rateLimit.lastSent = now
	}

	if e.rateLimit.sentCount >= e.config.RateLimit {
		return fmt.Errorf("rate limit exceeded: %d emails per minute", e.config.RateLimit)
	}

	e.rateLimit.sentCount++
	return nil
}

// sendEmailOnce sends a single email
func (e *EnhancedEmailManager) sendEmailOnce(subject, htmlBody, textBody string) error {
	// Build message with enhanced headers
	boundary := fmt.Sprintf("===============%d==", time.Now().UnixNano())

	msg := fmt.Sprintf("From: %s\r\n", e.config.From)
	msg += fmt.Sprintf("To: %s\r\n", strings.Join(e.config.To, ", "))
	if e.config.ReplyTo != "" {
		msg += fmt.Sprintf("Reply-To: %s\r\n", e.config.ReplyTo)
	}
	msg += fmt.Sprintf("Subject: %s\r\n", subject)
	msg += fmt.Sprintf("Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	msg += fmt.Sprintf("Message-ID: <%d@pulse-monitoring>\r\n", time.Now().UnixNano())
	msg += "MIME-Version: 1.0\r\n"
	msg += fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary)
	msg += "X-Mailer: Pulse Monitoring System\r\n"
	msg += "\r\n"

	// Text part
	msg += fmt.Sprintf("--%s\r\n", boundary)
	msg += "Content-Type: text/plain; charset=\"UTF-8\"\r\n"
	msg += "Content-Transfer-Encoding: 7bit\r\n"
	msg += "\r\n"
	msg += textBody + "\r\n"

	// HTML part
	msg += fmt.Sprintf("--%s\r\n", boundary)
	msg += "Content-Type: text/html; charset=\"UTF-8\"\r\n"
	msg += "Content-Transfer-Encoding: 7bit\r\n"
	msg += "\r\n"
	msg += htmlBody + "\r\n"

	// End boundary
	msg += fmt.Sprintf("--%s--\r\n", boundary)

	// Send based on provider configuration
	return e.sendViaProvider([]byte(msg))
}

// sendViaProvider sends email using provider-specific settings
func (e *EnhancedEmailManager) sendViaProvider(msg []byte) error {
	addr := fmt.Sprintf("%s:%d", e.config.SMTPHost, e.config.SMTPPort)

	// Special handling for specific providers
	switch e.config.Provider {
	case "SendGrid":
		// SendGrid uses "apikey" as username
		if e.config.Username == "" {
			e.config.Username = "apikey"
		}
	case "Postmark":
		// Postmark uses API token for both username and password
		if e.config.Password != "" && e.config.Username == "" {
			e.config.Username = e.config.Password
		}
	case "SparkPost":
		// SparkPost uses specific username
		if e.config.Username == "" {
			e.config.Username = "SMTP_Injection"
		}
	case "Resend":
		// Resend uses "resend" as username
		if e.config.Username == "" {
			e.config.Username = "resend"
		}
	}

	// Configure authentication
	var auth smtp.Auth
	if e.config.AuthRequired && e.config.Username != "" && e.config.Password != "" {
		auth = smtp.PlainAuth("", e.config.Username, e.config.Password, e.config.SMTPHost)
	}

	// Send with TLS configuration
	if e.config.TLS || e.config.SMTPPort == 465 {
		return e.sendTLS(addr, auth, msg)
	} else if e.config.StartTLS {
		return e.sendStartTLS(addr, auth, msg)
	} else {
		// Use sendPlain for non-TLS connections with timeout
		return e.sendPlain(addr, auth, msg)
	}
}

// sendTLS sends email over TLS connection
func (e *EnhancedEmailManager) sendTLS(addr string, auth smtp.Auth, msg []byte) error {
	tlsConfig := &tls.Config{
		ServerName:         e.config.SMTPHost,
		InsecureSkipVerify: e.config.SkipTLSVerify,
	}

	// Use DialWithDialer with timeout
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
	}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("TLS dial failed: %w", err)
	}
	defer conn.Close()

	// Set overall connection timeout
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	client, err := smtp.NewClient(conn, e.config.SMTPHost)
	if err != nil {
		return fmt.Errorf("SMTP client creation failed: %w", err)
	}
	defer client.Close()

	if auth != nil {
		if err = client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth failed: %w", err)
		}
	}

	if err = client.Mail(e.config.From); err != nil {
		return fmt.Errorf("MAIL FROM failed: %w", err)
	}

	for _, to := range e.config.To {
		if err = client.Rcpt(to); err != nil {
			return fmt.Errorf("RCPT TO failed for %s: %w", to, err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA command failed: %w", err)
	}

	_, err = w.Write(msg)
	if err != nil {
		return fmt.Errorf("message write failed: %w", err)
	}

	err = w.Close()
	if err != nil {
		return fmt.Errorf("message close failed: %w", err)
	}

	return client.Quit()
}

// sendStartTLS sends email using STARTTLS
func (e *EnhancedEmailManager) sendStartTLS(addr string, auth smtp.Auth, msg []byte) error {
	// Use DialTimeout to prevent hanging on unreachable servers
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("TCP dial failed: %w", err)
	}
	defer conn.Close()

	// Set overall connection timeout
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	client, err := smtp.NewClient(conn, e.config.SMTPHost)
	if err != nil {
		return fmt.Errorf("SMTP client creation failed: %w", err)
	}
	defer client.Close()

	// STARTTLS
	tlsConfig := &tls.Config{
		ServerName:         e.config.SMTPHost,
		InsecureSkipVerify: e.config.SkipTLSVerify,
	}

	if err = client.StartTLS(tlsConfig); err != nil {
		return fmt.Errorf("STARTTLS failed: %w", err)
	}

	if auth != nil {
		if err = client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth failed: %w", err)
		}
	}

	if err = client.Mail(e.config.From); err != nil {
		return fmt.Errorf("MAIL FROM failed: %w", err)
	}

	for _, to := range e.config.To {
		if err = client.Rcpt(to); err != nil {
			return fmt.Errorf("RCPT TO failed for %s: %w", to, err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA command failed: %w", err)
	}

	_, err = w.Write(msg)
	if err != nil {
		return fmt.Errorf("message write failed: %w", err)
	}

	err = w.Close()
	if err != nil {
		return fmt.Errorf("message close failed: %w", err)
	}

	return client.Quit()
}

// TestConnection tests the email server connection
func (e *EnhancedEmailManager) TestConnection() error {
	addr := fmt.Sprintf("%s:%d", e.config.SMTPHost, e.config.SMTPPort)

	// Try to connect
	var conn net.Conn
	var err error

	if e.config.TLS || e.config.SMTPPort == 465 {
		tlsConfig := &tls.Config{
			ServerName:         e.config.SMTPHost,
			InsecureSkipVerify: e.config.SkipTLSVerify,
		}
		conn, err = tls.Dial("tcp", addr, tlsConfig)
	} else {
		conn, err = net.DialTimeout("tcp", addr, 10*time.Second)
	}

	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, e.config.SMTPHost)
	if err != nil {
		return fmt.Errorf("SMTP handshake failed: %w", err)
	}
	defer client.Close()

	// Test STARTTLS if configured
	if e.config.StartTLS && !e.config.TLS {
		tlsConfig := &tls.Config{
			ServerName:         e.config.SMTPHost,
			InsecureSkipVerify: e.config.SkipTLSVerify,
		}
		if err = client.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("STARTTLS failed: %w", err)
		}
	}

	// Test authentication if configured
	if e.config.AuthRequired && e.config.Username != "" && e.config.Password != "" {
		auth := smtp.PlainAuth("", e.config.Username, e.config.Password, e.config.SMTPHost)
		if err = client.Auth(auth); err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}
	}

	return client.Quit()
}

// sendPlain sends email over plain SMTP connection with timeout
func (e *EnhancedEmailManager) sendPlain(addr string, auth smtp.Auth, msg []byte) error {
	// Use DialTimeout to prevent hanging on unreachable servers
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("TCP dial failed: %w", err)
	}
	defer conn.Close()

	// Set overall connection timeout
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	client, err := smtp.NewClient(conn, e.config.SMTPHost)
	if err != nil {
		return fmt.Errorf("SMTP client creation failed: %w", err)
	}
	defer client.Close()

	if auth != nil {
		if err = client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth failed: %w", err)
		}
	}

	if err = client.Mail(e.config.From); err != nil {
		return fmt.Errorf("MAIL FROM failed: %w", err)
	}

	for _, to := range e.config.To {
		if err = client.Rcpt(to); err != nil {
			return fmt.Errorf("RCPT TO failed for %s: %w", to, err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA command failed: %w", err)
	}

	_, err = w.Write(msg)
	if err != nil {
		return fmt.Errorf("message write failed: %w", err)
	}

	err = w.Close()
	if err != nil {
		return fmt.Errorf("message close failed: %w", err)
	}

	return client.Quit()
}
