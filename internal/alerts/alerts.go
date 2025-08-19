package alerts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rcourtman/pulse-go-rewrite/internal/models"
	"github.com/rcourtman/pulse-go-rewrite/internal/utils"
	"github.com/rs/zerolog/log"
)

// AlertLevel represents the severity of an alert
type AlertLevel string

const (
	AlertLevelWarning  AlertLevel = "warning"
	AlertLevelCritical AlertLevel = "critical"
)

// Alert represents an active alert
type Alert struct {
	ID           string                 `json:"id"`
	Type         string                 `json:"type"` // cpu, memory, disk, etc.
	Level        AlertLevel             `json:"level"`
	ResourceID   string                 `json:"resourceId"` // guest or node ID
	ResourceName string                 `json:"resourceName"`
	Node         string                 `json:"node"`
	Instance     string                 `json:"instance"`
	Message      string                 `json:"message"`
	Value        float64                `json:"value"`
	Threshold    float64                `json:"threshold"`
	StartTime    time.Time              `json:"startTime"`
	LastSeen     time.Time              `json:"lastSeen"`
	Acknowledged bool                   `json:"acknowledged"`
	AckTime      *time.Time             `json:"ackTime,omitempty"`
	AckUser      string                 `json:"ackUser,omitempty"`
	Metadata     map[string]interface{} `json:"metadata,omitempty"`
	// Escalation tracking
	LastEscalation  int         `json:"lastEscalation,omitempty"`  // Last escalation level notified
	EscalationTimes []time.Time `json:"escalationTimes,omitempty"` // Times when escalations were sent
}

// ResolvedAlert represents a recently resolved alert
type ResolvedAlert struct {
	*Alert
	ResolvedTime time.Time `json:"resolvedTime"`
}

// HysteresisThreshold represents a threshold with hysteresis
type HysteresisThreshold struct {
	Trigger float64 `json:"trigger"` // Threshold to trigger alert
	Clear   float64 `json:"clear"`   // Threshold to clear alert
}

// ThresholdConfig represents threshold configuration
type ThresholdConfig struct {
	CPU        *HysteresisThreshold `json:"cpu,omitempty"`
	Memory     *HysteresisThreshold `json:"memory,omitempty"`
	Disk       *HysteresisThreshold `json:"disk,omitempty"`
	DiskRead   *HysteresisThreshold `json:"diskRead,omitempty"`
	DiskWrite  *HysteresisThreshold `json:"diskWrite,omitempty"`
	NetworkIn  *HysteresisThreshold `json:"networkIn,omitempty"`
	NetworkOut *HysteresisThreshold `json:"networkOut,omitempty"`
	// Legacy fields for backward compatibility
	CPULegacy        *float64 `json:"cpuLegacy,omitempty"`
	MemoryLegacy     *float64 `json:"memoryLegacy,omitempty"`
	DiskLegacy       *float64 `json:"diskLegacy,omitempty"`
	DiskReadLegacy   *float64 `json:"diskReadLegacy,omitempty"`
	DiskWriteLegacy  *float64 `json:"diskWriteLegacy,omitempty"`
	NetworkInLegacy  *float64 `json:"networkInLegacy,omitempty"`
	NetworkOutLegacy *float64 `json:"networkOutLegacy,omitempty"`
}

// QuietHours represents quiet hours configuration
type QuietHours struct {
	Enabled  bool            `json:"enabled"`
	Start    string          `json:"start"` // 24-hour format "HH:MM"
	End      string          `json:"end"`   // 24-hour format "HH:MM"
	Timezone string          `json:"timezone"`
	Days     map[string]bool `json:"days"` // monday, tuesday, etc.
}

// EscalationLevel represents an escalation rule
type EscalationLevel struct {
	After  int    `json:"after"`  // minutes after initial alert
	Notify string `json:"notify"` // "email", "webhook", or "all"
}

// EscalationConfig represents alert escalation configuration
type EscalationConfig struct {
	Enabled bool              `json:"enabled"`
	Levels  []EscalationLevel `json:"levels"`
}

// GroupingConfig represents alert grouping configuration
type GroupingConfig struct {
	Enabled bool `json:"enabled"`
	Window  int  `json:"window"`  // seconds
	ByNode  bool `json:"byNode"`  // Group alerts by node
	ByGuest bool `json:"byGuest"` // Group alerts by guest type
}

// ScheduleConfig represents alerting schedule configuration
type ScheduleConfig struct {
	QuietHours     QuietHours       `json:"quietHours"`
	Cooldown       int              `json:"cooldown"`       // minutes
	GroupingWindow int              `json:"groupingWindow"` // seconds (deprecated, use Grouping.Window)
	MaxAlertsHour  int              `json:"maxAlertsHour"`  // max alerts per hour per resource
	Escalation     EscalationConfig `json:"escalation"`
	Grouping       GroupingConfig   `json:"grouping"`
}

// FilterCondition represents a single filter condition
type FilterCondition struct {
	Type     string      `json:"type"` // "metric", "text", or "raw"
	Field    string      `json:"field,omitempty"`
	Operator string      `json:"operator,omitempty"`
	Value    interface{} `json:"value,omitempty"`
	RawText  string      `json:"rawText,omitempty"`
}

// FilterStack represents a collection of filters with logical operator
type FilterStack struct {
	Filters         []FilterCondition `json:"filters"`
	LogicalOperator string            `json:"logicalOperator"` // "AND" or "OR"
}

// CustomAlertRule represents a custom alert rule with filter conditions
type CustomAlertRule struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Description      string          `json:"description,omitempty"`
	FilterConditions FilterStack     `json:"filterConditions"`
	Thresholds       ThresholdConfig `json:"thresholds"`
	Priority         int             `json:"priority"`
	Enabled          bool            `json:"enabled"`
	Notifications    struct {
		Email *struct {
			Enabled    bool     `json:"enabled"`
			Recipients []string `json:"recipients"`
		} `json:"email,omitempty"`
		Webhook *struct {
			Enabled bool   `json:"enabled"`
			URL     string `json:"url"`
		} `json:"webhook,omitempty"`
	} `json:"notifications"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// AlertConfig represents the complete alert configuration
type AlertConfig struct {
	Enabled        bool                       `json:"enabled"`
	GuestDefaults  ThresholdConfig            `json:"guestDefaults"`
	NodeDefaults   ThresholdConfig            `json:"nodeDefaults"`
	StorageDefault HysteresisThreshold        `json:"storageDefault"`
	Overrides      map[string]ThresholdConfig `json:"overrides"` // keyed by resource ID
	CustomRules    []CustomAlertRule          `json:"customRules,omitempty"`
	Schedule       ScheduleConfig             `json:"schedule"`
	// New configuration options
	MinimumDelta      float64 `json:"minimumDelta"`      // Minimum % change to trigger new alert
	SuppressionWindow int     `json:"suppressionWindow"` // Minutes to suppress duplicate alerts
	HysteresisMargin  float64 `json:"hysteresisMargin"`  // Default margin for legacy thresholds
	TimeThreshold     int     `json:"timeThreshold"`     // Seconds that threshold must be exceeded before triggering
}

// Manager handles alert monitoring and state
type Manager struct {
	mu             sync.RWMutex
	config         AlertConfig
	activeAlerts   map[string]*Alert
	historyManager *HistoryManager
	onAlert        func(alert *Alert)
	onResolved     func(alertID string)
	onEscalate     func(alert *Alert, level int)
	escalationStop chan struct{}
	alertRateLimit map[string][]time.Time // Track alert times for rate limiting
	// New fields for deduplication and suppression
	recentAlerts    map[string]*Alert    // Track recent alerts for deduplication
	suppressedUntil map[string]time.Time // Track suppression windows
	// Recently resolved alerts (kept for 5 minutes)
	recentlyResolved map[string]*ResolvedAlert
	resolvedMutex    sync.RWMutex
	// Time threshold tracking
	pendingAlerts map[string]time.Time // Track when thresholds were first exceeded
	// Node offline confirmation tracking
	nodeOfflineCount map[string]int // Track consecutive offline counts for nodes
}

// NewManager creates a new alert manager
func NewManager() *Manager {
	alertsDir := filepath.Join(utils.GetDataDir(), "alerts")
	m := &Manager{
		activeAlerts:     make(map[string]*Alert),
		historyManager:   NewHistoryManager(alertsDir),
		escalationStop:   make(chan struct{}),
		alertRateLimit:   make(map[string][]time.Time),
		recentAlerts:     make(map[string]*Alert),
		suppressedUntil:  make(map[string]time.Time),
		recentlyResolved: make(map[string]*ResolvedAlert),
		pendingAlerts:    make(map[string]time.Time),
		nodeOfflineCount: make(map[string]int),
		config: AlertConfig{
			Enabled: true,
			GuestDefaults: ThresholdConfig{
				CPU:        &HysteresisThreshold{Trigger: 80, Clear: 75},
				Memory:     &HysteresisThreshold{Trigger: 85, Clear: 80},
				Disk:       &HysteresisThreshold{Trigger: 90, Clear: 85},
				DiskRead:   &HysteresisThreshold{Trigger: 150, Clear: 125}, // 150 MB/s
				DiskWrite:  &HysteresisThreshold{Trigger: 150, Clear: 125}, // 150 MB/s
				NetworkIn:  &HysteresisThreshold{Trigger: 200, Clear: 175}, // 200 MB/s
				NetworkOut: &HysteresisThreshold{Trigger: 200, Clear: 175}, // 200 MB/s
			},
			NodeDefaults: ThresholdConfig{
				CPU:    &HysteresisThreshold{Trigger: 80, Clear: 75},
				Memory: &HysteresisThreshold{Trigger: 85, Clear: 80},
				Disk:   &HysteresisThreshold{Trigger: 90, Clear: 85},
			},
			StorageDefault:    HysteresisThreshold{Trigger: 85, Clear: 80},
			MinimumDelta:      2.0, // 2% minimum change
			SuppressionWindow: 5,   // 5 minutes
			HysteresisMargin:  5.0, // 5% default margin
			Overrides:         make(map[string]ThresholdConfig),
			Schedule: ScheduleConfig{
				QuietHours: QuietHours{
					Enabled:  false,
					Start:    "22:00",
					End:      "08:00",
					Timezone: "America/New_York",
					Days: map[string]bool{
						"monday":    true,
						"tuesday":   true,
						"wednesday": true,
						"thursday":  true,
						"friday":    true,
						"saturday":  false,
						"sunday":    false,
					},
				},
				Cooldown:       5,  // 5 minutes default
				GroupingWindow: 30, // 30 seconds default
				MaxAlertsHour:  10, // 10 alerts per hour default
				Escalation: EscalationConfig{
					Enabled: false,
					Levels: []EscalationLevel{
						{After: 15, Notify: "email"},
						{After: 30, Notify: "webhook"},
						{After: 60, Notify: "all"},
					},
				},
				Grouping: GroupingConfig{
					Enabled: true,
					Window:  30,
					ByNode:  true,
					ByGuest: false,
				},
			},
		},
	}

	// Load saved active alerts
	if err := m.LoadActiveAlerts(); err != nil {
		log.Error().Err(err).Msg("Failed to load active alerts")
	}

	// Start escalation checker
	go m.escalationChecker()

	// Start periodic save of active alerts
	go m.periodicSaveAlerts()

	return m
}

// SetAlertCallback sets the callback for new alerts
func (m *Manager) SetAlertCallback(cb func(alert *Alert)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onAlert = cb
}

// SetResolvedCallback sets the callback for resolved alerts
func (m *Manager) SetResolvedCallback(cb func(alertID string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onResolved = cb
}

// SetEscalateCallback sets the callback for escalated alerts
func (m *Manager) SetEscalateCallback(cb func(alert *Alert, level int)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onEscalate = cb
}

// UpdateConfig updates the alert configuration
func (m *Manager) UpdateConfig(config AlertConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Preserve defaults for zero values
	if config.StorageDefault.Trigger <= 0 {
		config.StorageDefault.Trigger = 85
		config.StorageDefault.Clear = 80
	}

	// Ensure minimums for other important fields
	if config.MinimumDelta <= 0 {
		config.MinimumDelta = 2.0
	}
	if config.SuppressionWindow <= 0 {
		config.SuppressionWindow = 5
	}
	if config.HysteresisMargin <= 0 {
		config.HysteresisMargin = 5.0
	}

	m.config = config
	log.Info().Msg("Alert configuration updated")
}

// isInQuietHours checks if the current time is within quiet hours
func (m *Manager) isInQuietHours() bool {
	if !m.config.Schedule.QuietHours.Enabled {
		return false
	}

	// Load timezone
	loc, err := time.LoadLocation(m.config.Schedule.QuietHours.Timezone)
	if err != nil {
		log.Warn().Err(err).Str("timezone", m.config.Schedule.QuietHours.Timezone).Msg("Failed to load timezone, using local time")
		loc = time.Local
	}

	now := time.Now().In(loc)
	dayName := strings.ToLower(now.Format("Monday"))

	// Check if today is enabled for quiet hours
	if enabled, ok := m.config.Schedule.QuietHours.Days[dayName]; !ok || !enabled {
		return false
	}

	// Parse start and end times
	startTime, err := time.ParseInLocation("15:04", m.config.Schedule.QuietHours.Start, loc)
	if err != nil {
		log.Warn().Err(err).Str("start", m.config.Schedule.QuietHours.Start).Msg("Failed to parse quiet hours start time")
		return false
	}

	endTime, err := time.ParseInLocation("15:04", m.config.Schedule.QuietHours.End, loc)
	if err != nil {
		log.Warn().Err(err).Str("end", m.config.Schedule.QuietHours.End).Msg("Failed to parse quiet hours end time")
		return false
	}

	// Set to today's date
	startTime = time.Date(now.Year(), now.Month(), now.Day(), startTime.Hour(), startTime.Minute(), 0, 0, loc)
	endTime = time.Date(now.Year(), now.Month(), now.Day(), endTime.Hour(), endTime.Minute(), 0, 0, loc)

	// Handle overnight quiet hours (e.g., 22:00 to 08:00)
	if endTime.Before(startTime) {
		// If we're past the start time or before the end time
		if now.After(startTime) || now.Before(endTime) {
			return true
		}
	} else {
		// Normal case (e.g., 08:00 to 17:00)
		if now.After(startTime) && now.Before(endTime) {
			return true
		}
	}

	return false
}

// GetConfig returns the current alert configuration
func (m *Manager) GetConfig() AlertConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// CheckGuest checks a guest (VM or container) against thresholds
func (m *Manager) CheckGuest(guest interface{}, instanceName string) {
	m.mu.RLock()
	if !m.config.Enabled {
		m.mu.RUnlock()
		return
	}
	m.mu.RUnlock()

	var guestID, name, node, guestType, status string
	var cpu, memUsage, diskUsage float64
	var diskRead, diskWrite, netIn, netOut int64

	// Extract data based on guest type
	switch g := guest.(type) {
	case models.VM:
		guestID = g.ID
		name = g.Name
		node = g.Node
		status = g.Status
		guestType = "VM"
		cpu = g.CPU // Already in percentage
		memUsage = g.Memory.Usage
		diskUsage = g.Disk.Usage
		diskRead = g.DiskRead
		diskWrite = g.DiskWrite
		netIn = g.NetworkIn
		netOut = g.NetworkOut
	case models.Container:
		guestID = g.ID
		name = g.Name
		node = g.Node
		status = g.Status
		guestType = "Container"
		cpu = g.CPU // Already in percentage
		memUsage = g.Memory.Usage
		diskUsage = g.Disk.Usage
		diskRead = g.DiskRead
		diskWrite = g.DiskWrite
		netIn = g.NetworkIn
		netOut = g.NetworkOut
	default:
		return
	}

	// Clear any alerts for stopped guests and skip threshold checks
	if status == "stopped" {
		// Clear all alerts for this guest if it's stopped
		m.mu.Lock()
		for alertID, alert := range m.activeAlerts {
			if alert.ResourceID == guestID {
				delete(m.activeAlerts, alertID)
				log.Info().
					Str("alertID", alertID).
					Str("guest", name).
					Msg("Cleared alert for stopped guest")
			}
		}
		m.mu.Unlock()
		return
	}

	// Get thresholds (check custom rules, then overrides, then defaults)
	m.mu.RLock()
	thresholds := m.getGuestThresholds(guest, guestID)
	m.mu.RUnlock()

	// Check each metric
	log.Info().
		Str("guest", name).
		Float64("cpu", cpu).
		Float64("memory", memUsage).
		Float64("disk", diskUsage).
		Interface("thresholds", thresholds).
		Msg("Checking guest thresholds")

	m.checkMetric(guestID, name, node, instanceName, guestType, "cpu", cpu, thresholds.CPU)
	m.checkMetric(guestID, name, node, instanceName, guestType, "memory", memUsage, thresholds.Memory)
	m.checkMetric(guestID, name, node, instanceName, guestType, "disk", diskUsage, thresholds.Disk)

	// Check I/O metrics (convert bytes/s to MB/s)
	if thresholds.DiskRead != nil && thresholds.DiskRead.Trigger > 0 {
		m.checkMetric(guestID, name, node, instanceName, guestType, "diskRead", float64(diskRead)/1024/1024, thresholds.DiskRead)
	}
	if thresholds.DiskWrite != nil && thresholds.DiskWrite.Trigger > 0 {
		m.checkMetric(guestID, name, node, instanceName, guestType, "diskWrite", float64(diskWrite)/1024/1024, thresholds.DiskWrite)
	}
	if thresholds.NetworkIn != nil && thresholds.NetworkIn.Trigger > 0 {
		m.checkMetric(guestID, name, node, instanceName, guestType, "networkIn", float64(netIn)/1024/1024, thresholds.NetworkIn)
	}
	if thresholds.NetworkOut != nil && thresholds.NetworkOut.Trigger > 0 {
		m.checkMetric(guestID, name, node, instanceName, guestType, "networkOut", float64(netOut)/1024/1024, thresholds.NetworkOut)
	}
}

// CheckNode checks a node against thresholds
func (m *Manager) CheckNode(node models.Node) {
	m.mu.RLock()
	if !m.config.Enabled {
		m.mu.RUnlock()
		return
	}
	thresholds := m.config.NodeDefaults
	m.mu.RUnlock()

	// CRITICAL: Check if node is offline first
	if node.Status == "offline" || node.ConnectionHealth == "error" || node.ConnectionHealth == "failed" {
		m.checkNodeOffline(node)
	} else {
		// Clear any existing offline alert if node is back online
		m.clearNodeOfflineAlert(node)
	}

	// Check each metric (only if node is online)
	if node.Status != "offline" {
		m.checkMetric(node.ID, node.Name, node.Name, node.Instance, "Node", "cpu", node.CPU*100, thresholds.CPU)
		m.checkMetric(node.ID, node.Name, node.Name, node.Instance, "Node", "memory", node.Memory.Usage, thresholds.Memory)
		m.checkMetric(node.ID, node.Name, node.Name, node.Instance, "Node", "disk", node.Disk.Usage, thresholds.Disk)
	}
}

// CheckStorage checks storage against thresholds
func (m *Manager) CheckStorage(storage models.Storage) {
	m.mu.RLock()
	if !m.config.Enabled {
		m.mu.RUnlock()
		return
	}
	threshold := m.config.StorageDefault
	m.mu.RUnlock()

	m.checkMetric(storage.ID, storage.Name, storage.Node, storage.Instance, "Storage", "usage", storage.Usage, &threshold)
}

// checkMetric checks a single metric against its threshold with hysteresis
func (m *Manager) checkMetric(resourceID, resourceName, node, instance, resourceType, metricType string, value float64, threshold *HysteresisThreshold) {
	if threshold == nil || threshold.Trigger <= 0 {
		return
	}

	log.Debug().
		Str("resource", resourceName).
		Str("metric", metricType).
		Float64("value", value).
		Float64("trigger", threshold.Trigger).
		Float64("clear", threshold.Clear).
		Bool("exceeds", value >= threshold.Trigger).
		Msg("Checking metric threshold")

	alertID := fmt.Sprintf("%s-%s", resourceID, metricType)

	m.mu.Lock()
	defer m.mu.Unlock()

	existingAlert, exists := m.activeAlerts[alertID]

	// Check for suppression
	if suppressUntil, suppressed := m.suppressedUntil[alertID]; suppressed && time.Now().Before(suppressUntil) {
		log.Debug().
			Str("alertID", alertID).
			Time("suppressedUntil", suppressUntil).
			Msg("Alert suppressed")
		return
	}

	if value >= threshold.Trigger {
		// Threshold exceeded
		if !exists {
			// Check if we have a time threshold configured
			if m.config.TimeThreshold > 0 {
				// Check if this threshold was already pending
				if pendingTime, isPending := m.pendingAlerts[alertID]; isPending {
					// Check if enough time has passed
					if time.Since(pendingTime) >= time.Duration(m.config.TimeThreshold)*time.Second {
						// Time threshold met, proceed with alert
						delete(m.pendingAlerts, alertID)
						log.Debug().
							Str("alertID", alertID).
							Int("timeThreshold", m.config.TimeThreshold).
							Dur("elapsed", time.Since(pendingTime)).
							Msg("Time threshold met, triggering alert")
					} else {
						// Still waiting for time threshold
						log.Debug().
							Str("alertID", alertID).
							Int("timeThreshold", m.config.TimeThreshold).
							Dur("elapsed", time.Since(pendingTime)).
							Msg("Threshold exceeded but waiting for time threshold")
						return
					}
				} else {
					// First time exceeding threshold, start tracking
					m.pendingAlerts[alertID] = time.Now()
					log.Debug().
						Str("alertID", alertID).
						Int("timeThreshold", m.config.TimeThreshold).
						Msg("Threshold exceeded, starting time threshold tracking")
					return
				}
			}

			// Check for recent similar alert to prevent spam
			if recent, hasRecent := m.recentAlerts[alertID]; hasRecent {
				// Check minimum delta
				if m.config.MinimumDelta > 0 &&
					time.Since(recent.StartTime) < time.Duration(m.config.SuppressionWindow)*time.Minute &&
					abs(recent.Value-value) < m.config.MinimumDelta {
					log.Debug().
						Str("alertID", alertID).
						Float64("recentValue", recent.Value).
						Float64("currentValue", value).
						Float64("delta", abs(recent.Value-value)).
						Float64("minimumDelta", m.config.MinimumDelta).
						Msg("Alert suppressed due to minimum delta")

					// Set suppression window
					m.suppressedUntil[alertID] = time.Now().Add(time.Duration(m.config.SuppressionWindow) * time.Minute)
					return
				}
			}

			// New alert
			alert := &Alert{
				ID:           alertID,
				Type:         metricType,
				Level:        AlertLevelWarning,
				ResourceID:   resourceID,
				ResourceName: resourceName,
				Node:         node,
				Instance:     instance,
				Message: func() string {
					if metricType == "usage" {
						return fmt.Sprintf("%s at %.1f%%", resourceType, value)
					}
					return fmt.Sprintf("%s %s at %.1f%%", resourceType, metricType, value)
				}(),
				Value:     value,
				Threshold: threshold.Trigger,
				StartTime: time.Now(),
				LastSeen:  time.Now(),
				Metadata: map[string]interface{}{
					"resourceType":   resourceType,
					"clearThreshold": threshold.Clear,
				},
			}

			// Set level based on how much over threshold
			if value >= threshold.Trigger+10 {
				alert.Level = AlertLevelCritical
			}

			m.activeAlerts[alertID] = alert
			m.recentAlerts[alertID] = alert
			m.historyManager.AddAlert(*alert)

			// Save active alerts after adding new one
			go func() {
				if err := m.SaveActiveAlerts(); err != nil {
					log.Error().Err(err).Msg("Failed to save active alerts after creation")
				}
			}()

			log.Warn().
				Str("alertID", alertID).
				Str("resource", resourceName).
				Str("metric", metricType).
				Float64("value", value).
				Float64("trigger", threshold.Trigger).
				Float64("clear", threshold.Clear).
				Int("activeAlerts", len(m.activeAlerts)).
				Msg("Alert triggered")

			// Check rate limit (but don't remove alert from tracking)
			if !m.checkRateLimit(alertID) {
				log.Debug().
					Str("alertID", alertID).
					Int("maxPerHour", m.config.Schedule.MaxAlertsHour).
					Msg("Alert notification suppressed due to rate limit")
				// Don't delete the alert, just suppress notifications
				return
			}

			// Check if we should suppress notifications due to quiet hours
			if m.isInQuietHours() && alert.Level != AlertLevelCritical {
				log.Debug().
					Str("alertID", alertID).
					Msg("Alert notification suppressed due to quiet hours (non-critical)")
			} else {
				// Notify callback
				if m.onAlert != nil {
					log.Info().Str("alertID", alertID).Msg("Calling onAlert callback")
					go m.onAlert(alert)
				} else {
					log.Warn().Msg("No onAlert callback set!")
				}
			}
		} else {
			// Update existing alert
			existingAlert.LastSeen = time.Now()
			existingAlert.Value = value

			// Update level if needed
			if value >= threshold.Trigger+10 {
				existingAlert.Level = AlertLevelCritical
			} else {
				existingAlert.Level = AlertLevelWarning
			}
		}
	} else {
		// Value is below trigger threshold
		// Clear any pending alert for this metric
		if _, isPending := m.pendingAlerts[alertID]; isPending {
			delete(m.pendingAlerts, alertID)
			log.Debug().
				Str("alertID", alertID).
				Msg("Value dropped below threshold, clearing pending alert")
		}

		if exists {
			// Use hysteresis for resolution - only resolve if below clear threshold
			clearThreshold := threshold.Clear
			if clearThreshold <= 0 {
				clearThreshold = threshold.Trigger // Fallback to trigger if clear not set
			}

			if value <= clearThreshold {
				// Threshold cleared with hysteresis - auto resolve
				resolvedAlert := &ResolvedAlert{
					Alert:        existingAlert,
					ResolvedTime: time.Now(),
				}

				// Remove from active alerts
				delete(m.activeAlerts, alertID)

				// Save active alerts after resolution
				go func() {
					if err := m.SaveActiveAlerts(); err != nil {
						log.Error().Err(err).Msg("Failed to save active alerts after resolution")
					}
				}()

				// Add to recently resolved
				m.resolvedMutex.Lock()
				m.recentlyResolved[alertID] = resolvedAlert
				m.resolvedMutex.Unlock()

				log.Info().
					Str("alertID", alertID).
					Int("totalRecentlyResolved", len(m.recentlyResolved)).
					Msg("Added alert to recently resolved")

				// Schedule cleanup after 5 minutes
				go func() {
					time.Sleep(5 * time.Minute)
					m.resolvedMutex.Lock()
					delete(m.recentlyResolved, alertID)
					m.resolvedMutex.Unlock()
				}()

				log.Info().
					Str("resource", resourceName).
					Str("metric", metricType).
					Float64("value", value).
					Float64("clearThreshold", clearThreshold).
					Bool("wasAcknowledged", existingAlert.Acknowledged).
					Msg("Alert resolved with hysteresis")

				if m.onResolved != nil {
					go m.onResolved(alertID)
				}
			}
		}
	}
}

// abs returns the absolute value of a float64
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// AcknowledgeAlert acknowledges an alert
func (m *Manager) AcknowledgeAlert(alertID, user string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	alert, exists := m.activeAlerts[alertID]
	if !exists {
		return fmt.Errorf("alert not found: %s", alertID)
	}

	alert.Acknowledged = true
	now := time.Now()
	alert.AckTime = &now
	alert.AckUser = user

	return nil
}

// GetActiveAlerts returns all active alerts
func (m *Manager) GetActiveAlerts() []Alert {
	m.mu.RLock()
	defer m.mu.RUnlock()

	alerts := make([]Alert, 0, len(m.activeAlerts))
	for _, alert := range m.activeAlerts {
		alerts = append(alerts, *alert)
	}
	return alerts
}

// GetRecentlyResolved returns recently resolved alerts
func (m *Manager) GetRecentlyResolved() []models.ResolvedAlert {
	m.resolvedMutex.RLock()
	defer m.resolvedMutex.RUnlock()

	resolved := make([]models.ResolvedAlert, 0, len(m.recentlyResolved))
	for _, alert := range m.recentlyResolved {
		resolved = append(resolved, models.ResolvedAlert{
			Alert: models.Alert{
				ID:           alert.ID,
				Type:         alert.Type,
				Level:        string(alert.Level),
				ResourceID:   alert.ResourceID,
				ResourceName: alert.ResourceName,
				Node:         alert.Node,
				Instance:     alert.Instance,
				Message:      alert.Message,
				Value:        alert.Value,
				Threshold:    alert.Threshold,
				StartTime:    alert.StartTime,
				Acknowledged: alert.Acknowledged,
			},
			ResolvedTime: alert.ResolvedTime,
		})
	}
	return resolved
}

// GetAlertHistory returns alert history
func (m *Manager) GetAlertHistory(limit int) []Alert {
	return m.historyManager.GetAllHistory(limit)
}

// ClearAlertHistory clears all alert history
func (m *Manager) ClearAlertHistory() error {
	return m.historyManager.ClearAllHistory()
}

// checkNodeOffline creates an alert for offline nodes after confirmation
func (m *Manager) checkNodeOffline(node models.Node) {
	alertID := fmt.Sprintf("node-offline-%s", node.ID)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if alert already exists
	if _, exists := m.activeAlerts[alertID]; exists {
		// Alert already exists, just update time
		m.activeAlerts[alertID].StartTime = time.Now()
		return
	}

	// Increment offline count
	m.nodeOfflineCount[node.ID]++
	offlineCount := m.nodeOfflineCount[node.ID]

	log.Debug().
		Str("node", node.Name).
		Str("instance", node.Instance).
		Int("offlineCount", offlineCount).
		Msg("Node offline detection count")

	// Require 3 consecutive offline polls (~15 seconds) before alerting
	// This prevents false positives from transient cluster communication issues
	const requiredOfflineCount = 3
	if offlineCount < requiredOfflineCount {
		log.Info().
			Str("node", node.Name).
			Int("count", offlineCount).
			Int("required", requiredOfflineCount).
			Msg("Node appears offline, waiting for confirmation")
		return
	}

	// Create new offline alert after confirmation
	alert := &Alert{
		ID:           alertID,
		Type:         "connectivity",
		Level:        AlertLevelCritical, // Node offline is always critical
		ResourceID:   node.ID,
		ResourceName: node.Name,
		Node:         node.Name,
		Instance:     node.Instance,
		Message:      fmt.Sprintf("Node '%s' is offline", node.Name),
		Value:        0, // Not applicable for offline status
		Threshold:    0, // Not applicable for offline status
		StartTime:    time.Now(),
		Acknowledged: false,
	}

	m.activeAlerts[alertID] = alert
	m.recentAlerts[alertID] = alert

	// Add to history
	m.historyManager.AddAlert(*alert)

	// Send notification after confirmation
	if m.onAlert != nil {
		m.onAlert(alert)
	}

	// Log the critical event
	log.Error().
		Str("node", node.Name).
		Str("instance", node.Instance).
		Str("status", node.Status).
		Str("connectionHealth", node.ConnectionHealth).
		Int("confirmedAfter", requiredOfflineCount).
		Msg("CRITICAL: Node is offline (confirmed)")
}

// clearNodeOfflineAlert removes offline alert when node comes back online
func (m *Manager) clearNodeOfflineAlert(node models.Node) {
	alertID := fmt.Sprintf("node-offline-%s", node.ID)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Reset offline count when node comes back online
	if m.nodeOfflineCount[node.ID] > 0 {
		log.Debug().
			Str("node", node.Name).
			Int("previousCount", m.nodeOfflineCount[node.ID]).
			Msg("Node back online, resetting offline count")
		delete(m.nodeOfflineCount, node.ID)
	}

	// Check if offline alert exists
	alert, exists := m.activeAlerts[alertID]
	if !exists {
		return
	}

	// Remove from active alerts
	delete(m.activeAlerts, alertID)

	// Add to recently resolved
	resolvedAlert := &ResolvedAlert{
		ResolvedTime: time.Now(),
	}
	resolvedAlert.Alert = alert
	m.recentlyResolved[alertID] = resolvedAlert

	// Send recovery notification
	if m.onResolved != nil {
		m.onResolved(alertID)
	}

	// Log recovery
	log.Info().
		Str("node", node.Name).
		Str("instance", node.Instance).
		Dur("downtime", time.Since(alert.StartTime)).
		Msg("Node is back online")
}

// ClearAlert manually clears an alert
func (m *Manager) ClearAlert(alertID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.activeAlerts, alertID)

	if m.onResolved != nil {
		go m.onResolved(alertID)
	}
}

// Cleanup removes old acknowledged alerts and cleans up tracking maps
func (m *Manager) Cleanup(maxAge time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()

	// Clean up acknowledged alerts
	for id, alert := range m.activeAlerts {
		if alert.Acknowledged && alert.AckTime != nil && now.Sub(*alert.AckTime) > maxAge {
			delete(m.activeAlerts, id)
		}
	}

	// Clean up recent alerts older than suppression window
	suppressionWindow := time.Duration(m.config.SuppressionWindow) * time.Minute
	if suppressionWindow == 0 {
		suppressionWindow = 5 * time.Minute // Default
	}

	for id, alert := range m.recentAlerts {
		if now.Sub(alert.StartTime) > suppressionWindow {
			delete(m.recentAlerts, id)
		}
	}

	// Clean up expired suppressions
	for id, suppressUntil := range m.suppressedUntil {
		if now.After(suppressUntil) {
			delete(m.suppressedUntil, id)
		}
	}
}

// convertLegacyThreshold converts a legacy float64 threshold to HysteresisThreshold
func (m *Manager) convertLegacyThreshold(legacy *float64) *HysteresisThreshold {
	if legacy == nil || *legacy <= 0 {
		return nil
	}
	margin := m.config.HysteresisMargin
	if margin <= 0 {
		margin = 5.0 // Default 5% margin
	}
	return &HysteresisThreshold{
		Trigger: *legacy,
		Clear:   *legacy - margin,
	}
}

// ensureHysteresisThreshold ensures a threshold has hysteresis configured
func ensureHysteresisThreshold(threshold *HysteresisThreshold) *HysteresisThreshold {
	if threshold == nil {
		return nil
	}
	if threshold.Clear <= 0 {
		threshold.Clear = threshold.Trigger - 5.0 // Default 5% margin
	}
	return threshold
}

// evaluateFilterCondition evaluates a single filter condition against a guest
func (m *Manager) evaluateFilterCondition(guest interface{}, condition FilterCondition) bool {
	switch g := guest.(type) {
	case models.VM:
		return m.evaluateVMCondition(g, condition)
	case models.Container:
		return m.evaluateContainerCondition(g, condition)
	default:
		return false
	}
}

// evaluateVMCondition evaluates a filter condition against a VM
func (m *Manager) evaluateVMCondition(vm models.VM, condition FilterCondition) bool {
	switch condition.Type {
	case "metric":
		value := 0.0
		switch strings.ToLower(condition.Field) {
		case "cpu":
			value = vm.CPU * 100
		case "memory":
			value = vm.Memory.Usage
		case "disk":
			value = vm.Disk.Usage
		case "diskread":
			value = float64(vm.DiskRead) / 1024 / 1024 // Convert to MB/s
		case "diskwrite":
			value = float64(vm.DiskWrite) / 1024 / 1024
		case "networkin":
			value = float64(vm.NetworkIn) / 1024 / 1024
		case "networkout":
			value = float64(vm.NetworkOut) / 1024 / 1024
		default:
			return false
		}

		condValue, ok := condition.Value.(float64)
		if !ok {
			// Try to convert from int
			if intVal, ok := condition.Value.(int); ok {
				condValue = float64(intVal)
			} else {
				return false
			}
		}

		switch condition.Operator {
		case ">":
			return value > condValue
		case "<":
			return value < condValue
		case ">=":
			return value >= condValue
		case "<=":
			return value <= condValue
		case "=", "==":
			return value >= condValue-0.5 && value <= condValue+0.5
		}

	case "text":
		searchValue := strings.ToLower(fmt.Sprintf("%v", condition.Value))
		switch strings.ToLower(condition.Field) {
		case "name":
			return strings.Contains(strings.ToLower(vm.Name), searchValue)
		case "node":
			return strings.Contains(strings.ToLower(vm.Node), searchValue)
		case "vmid":
			return strings.Contains(vm.ID, searchValue)
		}

	case "raw":
		if condition.RawText != "" {
			term := strings.ToLower(condition.RawText)
			return strings.Contains(strings.ToLower(vm.Name), term) ||
				strings.Contains(vm.ID, term) ||
				strings.Contains(strings.ToLower(vm.Node), term) ||
				strings.Contains(strings.ToLower(vm.Status), term)
		}
	}

	return false
}

// evaluateContainerCondition evaluates a filter condition against a Container
func (m *Manager) evaluateContainerCondition(ct models.Container, condition FilterCondition) bool {
	// Similar logic to evaluateVMCondition but for Container type
	switch condition.Type {
	case "metric":
		value := 0.0
		switch strings.ToLower(condition.Field) {
		case "cpu":
			value = ct.CPU * 100
		case "memory":
			value = ct.Memory.Usage
		case "disk":
			value = ct.Disk.Usage
		case "diskread":
			value = float64(ct.DiskRead) / 1024 / 1024
		case "diskwrite":
			value = float64(ct.DiskWrite) / 1024 / 1024
		case "networkin":
			value = float64(ct.NetworkIn) / 1024 / 1024
		case "networkout":
			value = float64(ct.NetworkOut) / 1024 / 1024
		default:
			return false
		}

		condValue, ok := condition.Value.(float64)
		if !ok {
			if intVal, ok := condition.Value.(int); ok {
				condValue = float64(intVal)
			} else {
				return false
			}
		}

		switch condition.Operator {
		case ">":
			return value > condValue
		case "<":
			return value < condValue
		case ">=":
			return value >= condValue
		case "<=":
			return value <= condValue
		case "=", "==":
			return value >= condValue-0.5 && value <= condValue+0.5
		}

	case "text":
		searchValue := strings.ToLower(fmt.Sprintf("%v", condition.Value))
		switch strings.ToLower(condition.Field) {
		case "name":
			return strings.Contains(strings.ToLower(ct.Name), searchValue)
		case "node":
			return strings.Contains(strings.ToLower(ct.Node), searchValue)
		case "vmid":
			return strings.Contains(ct.ID, searchValue)
		}

	case "raw":
		if condition.RawText != "" {
			term := strings.ToLower(condition.RawText)
			return strings.Contains(strings.ToLower(ct.Name), term) ||
				strings.Contains(ct.ID, term) ||
				strings.Contains(strings.ToLower(ct.Node), term) ||
				strings.Contains(strings.ToLower(ct.Status), term)
		}
	}

	return false
}

// evaluateFilterStack evaluates a filter stack against a guest
func (m *Manager) evaluateFilterStack(guest interface{}, stack FilterStack) bool {
	if len(stack.Filters) == 0 {
		return true
	}

	results := make([]bool, len(stack.Filters))
	for i, filter := range stack.Filters {
		results[i] = m.evaluateFilterCondition(guest, filter)
	}

	// Apply logical operator
	if stack.LogicalOperator == "AND" {
		for _, result := range results {
			if !result {
				return false
			}
		}
		return true
	} else { // OR
		for _, result := range results {
			if result {
				return true
			}
		}
		return false
	}
}

// getGuestThresholds returns the appropriate thresholds for a guest
// Priority: Guest-specific overrides > Custom rules (by priority) > Global defaults
func (m *Manager) getGuestThresholds(guest interface{}, guestID string) ThresholdConfig {
	// Start with defaults
	thresholds := m.config.GuestDefaults

	// Check custom rules (sorted by priority, highest first)
	var applicableRule *CustomAlertRule
	highestPriority := -1

	for i := range m.config.CustomRules {
		rule := &m.config.CustomRules[i]
		if !rule.Enabled {
			continue
		}

		// Check if this rule applies to the guest
		if m.evaluateFilterStack(guest, rule.FilterConditions) {
			if rule.Priority > highestPriority {
				applicableRule = rule
				highestPriority = rule.Priority
			}
		}
	}

	// Apply custom rule thresholds if found
	if applicableRule != nil {
		if applicableRule.Thresholds.CPU != nil {
			thresholds.CPU = ensureHysteresisThreshold(applicableRule.Thresholds.CPU)
		} else if applicableRule.Thresholds.CPULegacy != nil {
			thresholds.CPU = m.convertLegacyThreshold(applicableRule.Thresholds.CPULegacy)
		}
		if applicableRule.Thresholds.Memory != nil {
			thresholds.Memory = ensureHysteresisThreshold(applicableRule.Thresholds.Memory)
		} else if applicableRule.Thresholds.MemoryLegacy != nil {
			thresholds.Memory = m.convertLegacyThreshold(applicableRule.Thresholds.MemoryLegacy)
		}
		if applicableRule.Thresholds.Disk != nil {
			thresholds.Disk = ensureHysteresisThreshold(applicableRule.Thresholds.Disk)
		} else if applicableRule.Thresholds.DiskLegacy != nil {
			thresholds.Disk = m.convertLegacyThreshold(applicableRule.Thresholds.DiskLegacy)
		}
		if applicableRule.Thresholds.DiskRead != nil {
			thresholds.DiskRead = ensureHysteresisThreshold(applicableRule.Thresholds.DiskRead)
		} else if applicableRule.Thresholds.DiskReadLegacy != nil {
			thresholds.DiskRead = m.convertLegacyThreshold(applicableRule.Thresholds.DiskReadLegacy)
		}
		if applicableRule.Thresholds.DiskWrite != nil {
			thresholds.DiskWrite = ensureHysteresisThreshold(applicableRule.Thresholds.DiskWrite)
		} else if applicableRule.Thresholds.DiskWriteLegacy != nil {
			thresholds.DiskWrite = m.convertLegacyThreshold(applicableRule.Thresholds.DiskWriteLegacy)
		}
		if applicableRule.Thresholds.NetworkIn != nil {
			thresholds.NetworkIn = ensureHysteresisThreshold(applicableRule.Thresholds.NetworkIn)
		} else if applicableRule.Thresholds.NetworkInLegacy != nil {
			thresholds.NetworkIn = m.convertLegacyThreshold(applicableRule.Thresholds.NetworkInLegacy)
		}
		if applicableRule.Thresholds.NetworkOut != nil {
			thresholds.NetworkOut = ensureHysteresisThreshold(applicableRule.Thresholds.NetworkOut)
		} else if applicableRule.Thresholds.NetworkOutLegacy != nil {
			thresholds.NetworkOut = m.convertLegacyThreshold(applicableRule.Thresholds.NetworkOutLegacy)
		}

		log.Debug().
			Str("guest", guestID).
			Str("rule", applicableRule.Name).
			Int("priority", applicableRule.Priority).
			Msg("Applied custom alert rule")
	}

	// Finally check guest-specific overrides (highest priority)
	if override, exists := m.config.Overrides[guestID]; exists {
		if override.CPU != nil {
			thresholds.CPU = ensureHysteresisThreshold(override.CPU)
		} else if override.CPULegacy != nil {
			thresholds.CPU = m.convertLegacyThreshold(override.CPULegacy)
		}
		if override.Memory != nil {
			thresholds.Memory = ensureHysteresisThreshold(override.Memory)
		} else if override.MemoryLegacy != nil {
			thresholds.Memory = m.convertLegacyThreshold(override.MemoryLegacy)
		}
		if override.Disk != nil {
			thresholds.Disk = ensureHysteresisThreshold(override.Disk)
		} else if override.DiskLegacy != nil {
			thresholds.Disk = m.convertLegacyThreshold(override.DiskLegacy)
		}
		if override.DiskRead != nil {
			thresholds.DiskRead = ensureHysteresisThreshold(override.DiskRead)
		} else if override.DiskReadLegacy != nil {
			thresholds.DiskRead = m.convertLegacyThreshold(override.DiskReadLegacy)
		}
		if override.DiskWrite != nil {
			thresholds.DiskWrite = ensureHysteresisThreshold(override.DiskWrite)
		} else if override.DiskWriteLegacy != nil {
			thresholds.DiskWrite = m.convertLegacyThreshold(override.DiskWriteLegacy)
		}
		if override.NetworkIn != nil {
			thresholds.NetworkIn = ensureHysteresisThreshold(override.NetworkIn)
		} else if override.NetworkInLegacy != nil {
			thresholds.NetworkIn = m.convertLegacyThreshold(override.NetworkInLegacy)
		}
		if override.NetworkOut != nil {
			thresholds.NetworkOut = ensureHysteresisThreshold(override.NetworkOut)
		} else if override.NetworkOutLegacy != nil {
			thresholds.NetworkOut = m.convertLegacyThreshold(override.NetworkOutLegacy)
		}
	}

	return thresholds
}

// checkRateLimit checks if an alert has exceeded rate limit
func (m *Manager) checkRateLimit(alertID string) bool {
	if m.config.Schedule.MaxAlertsHour <= 0 {
		return true // No rate limit
	}

	now := time.Now()
	cutoff := now.Add(-1 * time.Hour)

	// Clean old entries and count recent alerts
	var recentAlerts []time.Time
	if times, exists := m.alertRateLimit[alertID]; exists {
		for _, t := range times {
			if t.After(cutoff) {
				recentAlerts = append(recentAlerts, t)
			}
		}
	}

	// Check if we've hit the limit
	if len(recentAlerts) >= m.config.Schedule.MaxAlertsHour {
		return false
	}

	// Add current time
	recentAlerts = append(recentAlerts, now)
	m.alertRateLimit[alertID] = recentAlerts

	return true
}

// escalationChecker runs periodically to check for alerts that need escalation and cleanup
func (m *Manager) escalationChecker() {
	ticker := time.NewTicker(1 * time.Minute)
	cleanupTicker := time.NewTicker(10 * time.Minute) // Run cleanup every 10 minutes
	defer ticker.Stop()
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ticker.C:
			m.checkEscalations()
		case <-cleanupTicker.C:
			m.Cleanup(24 * time.Hour) // Clean up acknowledged alerts older than 24 hours
		case <-m.escalationStop:
			return
		}
	}
}

// checkEscalations checks all active alerts for escalation
func (m *Manager) checkEscalations() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.config.Schedule.Escalation.Enabled {
		return
	}

	now := time.Now()
	for _, alert := range m.activeAlerts {
		// Skip acknowledged alerts
		if alert.Acknowledged {
			continue
		}

		// Check each escalation level
		for i, level := range m.config.Schedule.Escalation.Levels {
			// Skip if we've already escalated to this level
			if alert.LastEscalation >= i+1 {
				continue
			}

			// Check if it's time to escalate
			escalateTime := alert.StartTime.Add(time.Duration(level.After) * time.Minute)
			if now.After(escalateTime) {
				// Update alert escalation state
				alert.LastEscalation = i + 1
				alert.EscalationTimes = append(alert.EscalationTimes, now)

				log.Info().
					Str("alertID", alert.ID).
					Int("level", i+1).
					Str("notify", level.Notify).
					Msg("Alert escalated")

				// Trigger escalation callback
				if m.onEscalate != nil {
					go m.onEscalate(alert, i+1)
				}
			}
		}
	}
}

// Stop stops the alert manager and saves history
func (m *Manager) Stop() {
	close(m.escalationStop)
	m.historyManager.Stop()
	// Save active alerts before stopping
	if err := m.SaveActiveAlerts(); err != nil {
		log.Error().Err(err).Msg("Failed to save active alerts on stop")
	}
}

// SaveActiveAlerts persists active alerts to disk
func (m *Manager) SaveActiveAlerts() error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Create directory if it doesn't exist
	alertsDir := filepath.Join(utils.GetDataDir(), "alerts")
	if err := os.MkdirAll(alertsDir, 0755); err != nil {
		return fmt.Errorf("failed to create alerts directory: %w", err)
	}

	// Convert map to slice for JSON encoding
	alerts := make([]*Alert, 0, len(m.activeAlerts))
	for _, alert := range m.activeAlerts {
		alerts = append(alerts, alert)
	}

	data, err := json.MarshalIndent(alerts, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal active alerts: %w", err)
	}

	// Write to temporary file first, then rename (atomic operation)
	tmpFile := filepath.Join(alertsDir, "active-alerts.json.tmp")
	finalFile := filepath.Join(alertsDir, "active-alerts.json")

	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write active alerts: %w", err)
	}

	if err := os.Rename(tmpFile, finalFile); err != nil {
		return fmt.Errorf("failed to rename active alerts file: %w", err)
	}

	log.Info().Int("count", len(alerts)).Msg("Saved active alerts to disk")
	return nil
}

// LoadActiveAlerts restores active alerts from disk
func (m *Manager) LoadActiveAlerts() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	alertsFile := filepath.Join(utils.GetDataDir(), "alerts", "active-alerts.json")
	data, err := os.ReadFile(alertsFile)
	if err != nil {
		if os.IsNotExist(err) {
			log.Info().Msg("No active alerts file found, starting fresh")
			return nil
		}
		return fmt.Errorf("failed to read active alerts: %w", err)
	}

	var alerts []*Alert
	if err := json.Unmarshal(data, &alerts); err != nil {
		return fmt.Errorf("failed to unmarshal active alerts: %w", err)
	}

	// Restore alerts to the map
	now := time.Now()
	restoredCount := 0
	for _, alert := range alerts {
		// Skip very old alerts (older than 24 hours)
		if now.Sub(alert.StartTime) > 24*time.Hour {
			log.Debug().Str("alertID", alert.ID).Msg("Skipping old alert during restore")
			continue
		}

		// Skip acknowledged alerts older than 1 hour
		if alert.Acknowledged && alert.AckTime != nil && now.Sub(*alert.AckTime) > time.Hour {
			log.Debug().Str("alertID", alert.ID).Msg("Skipping old acknowledged alert")
			continue
		}

		m.activeAlerts[alert.ID] = alert
		restoredCount++
	}

	log.Info().Int("restored", restoredCount).Int("total", len(alerts)).Msg("Restored active alerts from disk")
	return nil
}

// periodicSaveAlerts saves active alerts to disk periodically
func (m *Manager) periodicSaveAlerts() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := m.SaveActiveAlerts(); err != nil {
				log.Error().Err(err).Msg("Failed to save active alerts during periodic save")
			}
		case <-m.escalationStop:
			return
		}
	}
}
