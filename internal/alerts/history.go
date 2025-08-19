package alerts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rcourtman/pulse-go-rewrite/internal/utils"
	"github.com/rs/zerolog/log"
)

const (
	// MaxHistoryDays is the maximum number of days to keep alert history
	MaxHistoryDays = 30
	// HistoryFileName is the name of the history file
	HistoryFileName = "alert-history.json"
	// HistoryBackupFileName is the name of the backup history file
	HistoryBackupFileName = "alert-history.backup.json"
)

// HistoryEntry represents a historical alert entry
type HistoryEntry struct {
	Alert     Alert     `json:"alert"`
	Timestamp time.Time `json:"timestamp"`
}

// HistoryManager manages persistent alert history
type HistoryManager struct {
	mu           sync.RWMutex
	dataDir      string
	historyFile  string
	backupFile   string
	history      []HistoryEntry
	saveInterval time.Duration
	stopChan     chan struct{}
	saveTicker   *time.Ticker
}

// NewHistoryManager creates a new history manager
func NewHistoryManager(dataDir string) *HistoryManager {
	if dataDir == "" {
		dataDir = utils.GetDataDir()
	}

	hm := &HistoryManager{
		dataDir:      dataDir,
		historyFile:  filepath.Join(dataDir, HistoryFileName),
		backupFile:   filepath.Join(dataDir, HistoryBackupFileName),
		history:      make([]HistoryEntry, 0),
		saveInterval: 5 * time.Minute,
		stopChan:     make(chan struct{}),
	}

	// Ensure data directory exists
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Error().Err(err).Str("dir", dataDir).Msg("Failed to create data directory")
	}

	// Load existing history
	if err := hm.loadHistory(); err != nil {
		log.Error().Err(err).Msg("Failed to load alert history")
	}

	// Start periodic save routine
	hm.startPeriodicSave()

	// Start cleanup routine
	go hm.cleanupRoutine()

	return hm
}

// AddAlert adds an alert to history
func (hm *HistoryManager) AddAlert(alert Alert) {
	hm.mu.Lock()
	defer hm.mu.Unlock()

	entry := HistoryEntry{
		Alert:     alert,
		Timestamp: time.Now(),
	}

	hm.history = append(hm.history, entry)
	log.Debug().Str("alertID", alert.ID).Msg("Added alert to history")
}

// GetHistory returns alert history within the specified time range
func (hm *HistoryManager) GetHistory(since time.Time, limit int) []Alert {
	hm.mu.RLock()
	defer hm.mu.RUnlock()

	var results []Alert
	count := 0

	// Iterate from newest to oldest
	for i := len(hm.history) - 1; i >= 0 && (limit <= 0 || count < limit); i-- {
		entry := hm.history[i]
		if entry.Timestamp.After(since) {
			results = append(results, entry.Alert)
			count++
		}
	}

	return results
}

// GetAllHistory returns all alert history (up to limit)
func (hm *HistoryManager) GetAllHistory(limit int) []Alert {
	hm.mu.RLock()
	defer hm.mu.RUnlock()

	if limit <= 0 || limit > len(hm.history) {
		limit = len(hm.history)
	}

	results := make([]Alert, 0, limit)
	start := len(hm.history) - limit

	for i := len(hm.history) - 1; i >= start; i-- {
		results = append(results, hm.history[i].Alert)
	}

	return results
}

// loadHistory loads history from disk
func (hm *HistoryManager) loadHistory() error {
	// Try loading from main file first
	data, err := os.ReadFile(hm.historyFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn().Err(err).Str("file", hm.historyFile).Msg("Failed to read history file")
		}

		// Try backup file
		data, err = os.ReadFile(hm.backupFile)
		if err != nil {
			if os.IsNotExist(err) {
				// Both files don't exist - this is normal on first startup
				log.Debug().Msg("No alert history files found, starting fresh")
				return nil
			}
			// Check if it's a permission error
			if os.IsPermission(err) {
				log.Warn().Err(err).Str("file", hm.backupFile).Msg("Permission denied reading backup history file - check file ownership")
				return nil // Continue without history rather than failing
			}
			return fmt.Errorf("failed to load backup history: %w", err)
		}
		log.Info().Msg("Loaded alert history from backup file")
	}

	var history []HistoryEntry
	if err := json.Unmarshal(data, &history); err != nil {
		return fmt.Errorf("failed to unmarshal history: %w", err)
	}

	hm.history = history
	log.Info().Int("count", len(history)).Msg("Loaded alert history")

	// Clean old entries immediately
	hm.cleanOldEntries()

	return nil
}

// saveHistory saves history to disk
func (hm *HistoryManager) saveHistory() error {
	hm.mu.RLock()
	data, err := json.MarshalIndent(hm.history, "", "  ")
	hm.mu.RUnlock()

	if err != nil {
		return fmt.Errorf("failed to marshal history: %w", err)
	}

	// Create backup of existing file
	if _, err := os.Stat(hm.historyFile); err == nil {
		if err := os.Rename(hm.historyFile, hm.backupFile); err != nil {
			log.Warn().Err(err).Msg("Failed to create backup file")
		}
	}

	// Write new file
	if err := os.WriteFile(hm.historyFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write history file: %w", err)
	}

	log.Debug().Int("entries", len(hm.history)).Msg("Saved alert history")
	return nil
}

// startPeriodicSave starts the periodic save routine
func (hm *HistoryManager) startPeriodicSave() {
	hm.saveTicker = time.NewTicker(hm.saveInterval)

	go func() {
		for {
			select {
			case <-hm.saveTicker.C:
				if err := hm.saveHistory(); err != nil {
					log.Error().Err(err).Msg("Failed to save alert history")
				}
			case <-hm.stopChan:
				return
			}
		}
	}()
}

// cleanupRoutine runs periodically to clean old entries
func (hm *HistoryManager) cleanupRoutine() {
	// Run cleanup daily
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	// Also run cleanup on startup after a delay
	time.Sleep(1 * time.Minute)
	hm.cleanOldEntries()

	for {
		select {
		case <-ticker.C:
			hm.cleanOldEntries()
		case <-hm.stopChan:
			return
		}
	}
}

// cleanOldEntries removes entries older than MaxHistoryDays
func (hm *HistoryManager) cleanOldEntries() {
	hm.mu.Lock()
	defer hm.mu.Unlock()

	cutoff := time.Now().AddDate(0, 0, -MaxHistoryDays)
	newHistory := make([]HistoryEntry, 0, len(hm.history))

	removed := 0
	for _, entry := range hm.history {
		if entry.Timestamp.After(cutoff) {
			newHistory = append(newHistory, entry)
		} else {
			removed++
		}
	}

	if removed > 0 {
		hm.history = newHistory
		log.Info().
			Int("removed", removed).
			Int("remaining", len(newHistory)).
			Msg("Cleaned old alert history entries")
	}
}

// ClearAllHistory clears all alert history
func (hm *HistoryManager) ClearAllHistory() error {
	hm.mu.Lock()
	defer hm.mu.Unlock()

	// Clear the in-memory history
	hm.history = make([]HistoryEntry, 0)

	// Remove the history files
	_ = os.Remove(hm.historyFile)
	_ = os.Remove(hm.backupFile)

	log.Info().Msg("Cleared all alert history")
	return nil
}

// Stop stops the history manager
func (hm *HistoryManager) Stop() {
	close(hm.stopChan)
	if hm.saveTicker != nil {
		hm.saveTicker.Stop()
	}

	// Save one final time
	if err := hm.saveHistory(); err != nil {
		log.Error().Err(err).Msg("Failed to save alert history on shutdown")
	}
}

// GetStats returns statistics about the alert history
func (hm *HistoryManager) GetStats() map[string]interface{} {
	hm.mu.RLock()
	defer hm.mu.RUnlock()

	oldest := time.Now()
	newest := time.Time{}

	if len(hm.history) > 0 {
		oldest = hm.history[0].Timestamp
		newest = hm.history[len(hm.history)-1].Timestamp
	}

	return map[string]interface{}{
		"totalEntries": len(hm.history),
		"oldestEntry":  oldest,
		"newestEntry":  newest,
		"dataDir":      hm.dataDir,
		"fileSize":     hm.getFileSize(),
	}
}

// getFileSize returns the size of the history file
func (hm *HistoryManager) getFileSize() int64 {
	info, err := os.Stat(hm.historyFile)
	if err != nil {
		return 0
	}
	return info.Size()
}
