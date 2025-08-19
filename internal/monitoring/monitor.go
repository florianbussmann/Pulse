package monitoring

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rcourtman/pulse-go-rewrite/internal/alerts"
	"github.com/rcourtman/pulse-go-rewrite/internal/config"
	"github.com/rcourtman/pulse-go-rewrite/internal/discovery"
	"github.com/rcourtman/pulse-go-rewrite/internal/errors"
	"github.com/rcourtman/pulse-go-rewrite/internal/models"
	"github.com/rcourtman/pulse-go-rewrite/internal/notifications"
	"github.com/rcourtman/pulse-go-rewrite/internal/websocket"
	"github.com/rcourtman/pulse-go-rewrite/pkg/pbs"
	"github.com/rcourtman/pulse-go-rewrite/pkg/proxmox"
	"github.com/rs/zerolog/log"
)

// PVEClientInterface defines the interface for PVE clients (both regular and cluster)
type PVEClientInterface interface {
	GetNodes(ctx context.Context) ([]proxmox.Node, error)
	GetNodeStatus(ctx context.Context, node string) (*proxmox.NodeStatus, error)
	GetVMs(ctx context.Context, node string) ([]proxmox.VM, error)
	GetContainers(ctx context.Context, node string) ([]proxmox.Container, error)
	GetStorage(ctx context.Context, node string) ([]proxmox.Storage, error)
	GetAllStorage(ctx context.Context) ([]proxmox.Storage, error)
	GetBackupTasks(ctx context.Context) ([]proxmox.Task, error)
	GetStorageContent(ctx context.Context, node, storage string) ([]proxmox.StorageContent, error)
	GetVMSnapshots(ctx context.Context, node string, vmid int) ([]proxmox.Snapshot, error)
	GetContainerSnapshots(ctx context.Context, node string, vmid int) ([]proxmox.Snapshot, error)
	GetVMStatus(ctx context.Context, node string, vmid int) (*proxmox.VMStatus, error)
	GetContainerStatus(ctx context.Context, node string, vmid int) (*proxmox.Container, error)
	GetClusterResources(ctx context.Context, resourceType string) ([]proxmox.ClusterResource, error)
	IsClusterMember(ctx context.Context) (bool, error)
}

// Monitor handles all monitoring operations
type Monitor struct {
	config           *config.Config
	state            *models.State
	pveClients       map[string]PVEClientInterface
	pbsClients       map[string]*pbs.Client
	mu               sync.RWMutex
	startTime        time.Time
	rateTracker      *RateTracker
	metricsHistory   *MetricsHistory
	alertManager     *alerts.Manager
	notificationMgr  *notifications.NotificationManager
	configPersist    *config.ConfigPersistence
	discoveryService *discovery.Service   // Background discovery service
	activePollCount  int32                // Number of active polling operations
	pollCounter      int64                // Counter for polling cycles
	authFailures     map[string]int       // Track consecutive auth failures per node
	lastAuthAttempt  map[string]time.Time // Track last auth attempt time
}

// safePercentage calculates percentage safely, returning 0 if divisor is 0
func safePercentage(used, total float64) float64 {
	if total == 0 {
		return 0
	}
	result := used / total * 100
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0
	}
	return result
}

// maxInt64 returns the maximum of two int64 values
func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// safeFloat ensures a float value is not NaN or Inf
func safeFloat(val float64) float64 {
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0
	}
	return val
}

// sortContent sorts comma-separated content values for consistent display
func sortContent(content string) string {
	if content == "" {
		return ""
	}
	parts := strings.Split(content, ",")
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// GetConnectionStatuses returns the current connection status for all nodes
func (m *Monitor) GetConnectionStatuses() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make(map[string]bool)

	// Check PVE clients
	for name, client := range m.pveClients {
		// Simple check - if we have a client, consider it connected
		// In reality, you'd want to check if recent API calls succeeded
		statuses["pve-"+name] = client != nil
	}

	// Check PBS clients
	for name, client := range m.pbsClients {
		statuses["pbs-"+name] = client != nil
	}

	return statuses
}

// New creates a new Monitor instance
func New(cfg *config.Config) (*Monitor, error) {
	m := &Monitor{
		config:           cfg,
		state:            models.NewState(),
		pveClients:       make(map[string]PVEClientInterface),
		pbsClients:       make(map[string]*pbs.Client),
		startTime:        time.Now(),
		rateTracker:      NewRateTracker(),
		metricsHistory:   NewMetricsHistory(1000, 24*time.Hour), // Keep up to 1000 points or 24 hours
		alertManager:     alerts.NewManager(),
		notificationMgr:  notifications.NewNotificationManager(),
		configPersist:    config.NewConfigPersistence(cfg.DataPath),
		discoveryService: nil, // Will be initialized in Start()
		authFailures:     make(map[string]int),
		lastAuthAttempt:  make(map[string]time.Time),
	}

	// Load saved configurations
	if alertConfig, err := m.configPersist.LoadAlertConfig(); err == nil {
		m.alertManager.UpdateConfig(*alertConfig)
		// Apply schedule settings to notification manager
		if alertConfig.Schedule.Cooldown > 0 {
			m.notificationMgr.SetCooldown(alertConfig.Schedule.Cooldown)
		}
		if alertConfig.Schedule.GroupingWindow > 0 {
			m.notificationMgr.SetGroupingWindow(alertConfig.Schedule.GroupingWindow)
		} else if alertConfig.Schedule.Grouping.Window > 0 {
			m.notificationMgr.SetGroupingWindow(alertConfig.Schedule.Grouping.Window)
		}
		m.notificationMgr.SetGroupingOptions(
			alertConfig.Schedule.Grouping.ByNode,
			alertConfig.Schedule.Grouping.ByGuest,
		)
	} else {
		log.Warn().Err(err).Msg("Failed to load alert configuration")
	}

	if emailConfig, err := m.configPersist.LoadEmailConfig(); err == nil {
		m.notificationMgr.SetEmailConfig(*emailConfig)
	} else {
		log.Warn().Err(err).Msg("Failed to load email configuration")
	}

	if webhooks, err := m.configPersist.LoadWebhooks(); err == nil {
		for _, webhook := range webhooks {
			m.notificationMgr.AddWebhook(webhook)
		}
	} else {
		log.Warn().Err(err).Msg("Failed to load webhook configuration")
	}

	// Initialize PVE clients
	log.Info().Int("count", len(cfg.PVEInstances)).Msg("Initializing PVE clients")
	for _, pve := range cfg.PVEInstances {
		log.Info().
			Str("name", pve.Name).
			Str("host", pve.Host).
			Str("user", pve.User).
			Bool("hasToken", pve.TokenName != "").
			Msg("Configuring PVE instance")

		// Check if this is a cluster
		if pve.IsCluster && len(pve.ClusterEndpoints) > 0 {
			// Create cluster client
			endpoints := make([]string, 0, len(pve.ClusterEndpoints))
			for _, ep := range pve.ClusterEndpoints {
				// Use IP if available, otherwise use host
				host := ep.IP
				if host == "" {
					host = ep.Host
				}

				// Skip if no host information
				if host == "" {
					log.Warn().
						Str("node", ep.NodeName).
						Msg("Skipping cluster endpoint with no host/IP")
					continue
				}

				// Ensure we have the full URL
				if !strings.HasPrefix(host, "http") {
					if pve.VerifySSL {
						host = fmt.Sprintf("https://%s:8006", host)
					} else {
						host = fmt.Sprintf("https://%s:8006", host)
					}
				}
				endpoints = append(endpoints, host)
			}

			// If no valid endpoints, fall back to single node mode
			if len(endpoints) == 0 {
				log.Warn().
					Str("instance", pve.Name).
					Msg("No valid cluster endpoints found, falling back to single node mode")
				endpoints = []string{pve.Host}
				if !strings.HasPrefix(endpoints[0], "http") {
					endpoints[0] = fmt.Sprintf("https://%s:8006", endpoints[0])
				}
			}

			log.Info().
				Str("cluster", pve.ClusterName).
				Strs("endpoints", endpoints).
				Msg("Creating cluster-aware client")

			clientConfig := config.CreateProxmoxConfig(&pve)
			clientConfig.Timeout = cfg.ConnectionTimeout
			clusterClient := proxmox.NewClusterClient(
				pve.Name,
				clientConfig,
				endpoints,
			)
			m.pveClients[pve.Name] = clusterClient
			log.Info().
				Str("instance", pve.Name).
				Str("cluster", pve.ClusterName).
				Int("endpoints", len(endpoints)).
				Msg("Cluster client created successfully")
		} else {
			// Create regular client
			clientConfig := config.CreateProxmoxConfig(&pve)
			clientConfig.Timeout = cfg.ConnectionTimeout
			client, err := proxmox.NewClient(clientConfig)
			if err != nil {
				monErr := errors.WrapConnectionError("create_pve_client", pve.Name, err)
				log.Error().Err(monErr).Str("instance", pve.Name).Msg("Failed to create PVE client")
				continue
			}
			m.pveClients[pve.Name] = client
			log.Info().Str("instance", pve.Name).Msg("PVE client created successfully")
		}
	}

	// Initialize PBS clients
	log.Info().Int("count", len(cfg.PBSInstances)).Msg("Initializing PBS clients")
	for _, pbsInst := range cfg.PBSInstances {
		log.Info().
			Str("name", pbsInst.Name).
			Str("host", pbsInst.Host).
			Str("user", pbsInst.User).
			Bool("hasToken", pbsInst.TokenName != "").
			Msg("Configuring PBS instance")

		clientConfig := config.CreatePBSConfig(&pbsInst)
		clientConfig.Timeout = 60 * time.Second // Very generous timeout for slow PBS servers
		client, err := pbs.NewClient(clientConfig)
		if err != nil {
			monErr := errors.WrapConnectionError("create_pbs_client", pbsInst.Name, err)
			log.Error().Err(monErr).Str("instance", pbsInst.Name).Msg("Failed to create PBS client")
			continue
		}
		m.pbsClients[pbsInst.Name] = client
		log.Info().Str("instance", pbsInst.Name).Msg("PBS client created successfully")
	}

	// Initialize state stats
	m.state.Stats = models.Stats{
		StartTime: m.startTime,
		Version:   "2.0.0-go",
	}

	return m, nil
}

// Start begins the monitoring loop
func (m *Monitor) Start(ctx context.Context, wsHub *websocket.Hub) {
	log.Info().
		Dur("pollingInterval", 10*time.Second).
		Msg("Starting monitoring loop")

	// Initialize and start discovery service
	discoverySubnet := m.config.DiscoverySubnet
	if discoverySubnet == "" {
		discoverySubnet = "auto"
	}
	m.discoveryService = discovery.NewService(wsHub, 5*time.Minute, discoverySubnet)
	if m.discoveryService != nil {
		m.discoveryService.Start(ctx)
		log.Info().Msg("Discovery service initialized and started")
	} else {
		log.Error().Msg("Failed to initialize discovery service")
	}

	// Set up alert callbacks
	m.alertManager.SetAlertCallback(func(alert *alerts.Alert) {
		wsHub.BroadcastAlert(alert)
		// Send notifications
		log.Debug().
			Str("alertID", alert.ID).
			Str("level", string(alert.Level)).
			Msg("Alert raised, sending to notification manager")
		go m.notificationMgr.SendAlert(alert)
	})
	m.alertManager.SetResolvedCallback(func(alertID string) {
		wsHub.BroadcastAlertResolved(alertID)
		// Broadcast updated state immediately so frontend gets the new activeAlerts list
		state := m.GetState()
		wsHub.BroadcastState(state)
	})
	m.alertManager.SetEscalateCallback(func(alert *alerts.Alert, level int) {
		log.Info().
			Str("alertID", alert.ID).
			Int("level", level).
			Msg("Alert escalated - sending notifications")

		// Get escalation config
		config := m.alertManager.GetConfig()
		if level <= 0 || level > len(config.Schedule.Escalation.Levels) {
			return
		}

		escalationLevel := config.Schedule.Escalation.Levels[level-1]

		// Send notifications based on escalation level
		switch escalationLevel.Notify {
		case "email":
			// Only send email
			if emailConfig := m.notificationMgr.GetEmailConfig(); emailConfig.Enabled {
				m.notificationMgr.SendAlert(alert)
			}
		case "webhook":
			// Only send webhooks
			for _, webhook := range m.notificationMgr.GetWebhooks() {
				if webhook.Enabled {
					m.notificationMgr.SendAlert(alert)
					break
				}
			}
		case "all":
			// Send all notifications
			m.notificationMgr.SendAlert(alert)
		}

		// Update WebSocket with escalation
		wsHub.BroadcastAlert(alert)
	})

	// Create separate tickers for polling and broadcasting
	// Hardcoded to 10 seconds since Proxmox updates cluster/resources every 10 seconds
	const pollingInterval = 10 * time.Second
	pollTicker := time.NewTicker(pollingInterval)
	defer pollTicker.Stop()

	broadcastTicker := time.NewTicker(pollingInterval)
	defer broadcastTicker.Stop()

	// Do an immediate poll on start
	go m.poll(ctx, wsHub)

	for {
		select {
		case <-pollTicker.C:
			// Start polling in a goroutine so it doesn't block the ticker
			go m.poll(ctx, wsHub)

		case <-broadcastTicker.C:
			// Broadcast current state regardless of polling status
			state := m.state.GetSnapshot()
			log.Info().
				Int("nodes", len(state.Nodes)).
				Int("vms", len(state.VMs)).
				Int("containers", len(state.Containers)).
				Int("pbs", len(state.PBSInstances)).
				Int("pbsBackups", len(state.PBSBackups)).
				Msg("Broadcasting state update (ticker)")
			wsHub.BroadcastState(state)

		case <-ctx.Done():
			log.Info().Msg("Monitoring loop stopped")
			return
		}
	}
}

// poll fetches data from all configured instances
func (m *Monitor) poll(ctx context.Context, wsHub *websocket.Hub) {
	// Limit concurrent polls to 2 to prevent resource exhaustion
	currentCount := atomic.AddInt32(&m.activePollCount, 1)
	if currentCount > 2 {
		atomic.AddInt32(&m.activePollCount, -1)
		log.Debug().Int32("activePolls", currentCount-1).Msg("Too many concurrent polls, skipping")
		return
	}
	defer atomic.AddInt32(&m.activePollCount, -1)

	log.Debug().Msg("Starting polling cycle")
	startTime := time.Now()

	if m.config.ConcurrentPolling {
		// Use concurrent polling
		m.pollConcurrent(ctx)
	} else {
		m.pollSequential(ctx)
	}

	// Update performance metrics
	m.state.Performance.LastPollDuration = time.Since(startTime).Seconds()
	m.state.Stats.PollingCycles++
	m.state.Stats.Uptime = int64(time.Since(m.startTime).Seconds())
	m.state.Stats.WebSocketClients = wsHub.GetClientCount()

	// Sync active alerts to state
	activeAlerts := m.alertManager.GetActiveAlerts()
	modelAlerts := make([]models.Alert, 0, len(activeAlerts))
	for _, alert := range activeAlerts {
		modelAlerts = append(modelAlerts, models.Alert{
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
		})
	}
	m.state.UpdateActiveAlerts(modelAlerts)

	// Sync recently resolved alerts
	recentlyResolved := m.alertManager.GetRecentlyResolved()
	if len(recentlyResolved) > 0 {
		log.Info().Int("count", len(recentlyResolved)).Msg("Syncing recently resolved alerts")
	}
	m.state.UpdateRecentlyResolved(recentlyResolved)

	// Increment poll counter
	m.mu.Lock()
	m.pollCounter++
	m.mu.Unlock()

	log.Debug().Dur("duration", time.Since(startTime)).Msg("Polling cycle completed")

	// Broadcasting is now handled by the timer in Start()
}

// pollConcurrent polls all instances concurrently
func (m *Monitor) pollConcurrent(ctx context.Context) {
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Poll PVE instances
	for name, client := range m.pveClients {
		// Check if context is already cancelled before starting
		select {
		case <-ctx.Done():
			return
		default:
		}

		wg.Add(1)
		go func(instanceName string, c PVEClientInterface) {
			defer wg.Done()
			// Pass context to ensure cancellation propagates
			m.pollPVEInstance(ctx, instanceName, c)
		}(name, client)
	}

	// Poll PBS instances
	for name, client := range m.pbsClients {
		// Check if context is already cancelled before starting
		select {
		case <-ctx.Done():
			return
		default:
		}

		wg.Add(1)
		go func(instanceName string, c *pbs.Client) {
			defer wg.Done()
			// Pass context to ensure cancellation propagates
			m.pollPBSInstance(ctx, instanceName, c)
		}(name, client)
	}

	// Wait for all goroutines to complete or context cancellation
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All goroutines completed normally
	case <-ctx.Done():
		// Context cancelled, cancel all operations
		cancel()
		// Still wait for goroutines to finish gracefully
		wg.Wait()
	}
}

// pollSequential polls all instances sequentially
func (m *Monitor) pollSequential(ctx context.Context) {
	// Poll PVE instances
	for name, client := range m.pveClients {
		// Check context before each instance
		select {
		case <-ctx.Done():
			return
		default:
		}
		m.pollPVEInstance(ctx, name, client)
	}

	// Poll PBS instances
	for name, client := range m.pbsClients {
		// Check context before each instance
		select {
		case <-ctx.Done():
			return
		default:
		}
		m.pollPBSInstance(ctx, name, client)
	}
}

// pollPVEInstance polls a single PVE instance
func (m *Monitor) pollPVEInstance(ctx context.Context, instanceName string, client PVEClientInterface) {
	// Check if context is cancelled
	select {
	case <-ctx.Done():
		log.Debug().Str("instance", instanceName).Msg("Polling cancelled")
		return
	default:
	}

	log.Debug().Str("instance", instanceName).Msg("Polling PVE instance")

	// Get instance config
	var instanceCfg *config.PVEInstance
	for _, cfg := range m.config.PVEInstances {
		if cfg.Name == instanceName {
			instanceCfg = &cfg
			break
		}
	}
	if instanceCfg == nil {
		return
	}

	// Poll nodes
	nodes, err := client.GetNodes(ctx)
	if err != nil {
		monErr := errors.WrapConnectionError("poll_nodes", instanceName, err)
		log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to get nodes")
		m.state.SetConnectionHealth(instanceName, false)

		// Track auth failure if it's an authentication error
		if errors.IsAuthError(err) {
			m.recordAuthFailure(instanceName, "pve")
		}
		return
	}

	// Reset auth failures on successful connection
	m.resetAuthFailures(instanceName, "pve")
	m.state.SetConnectionHealth(instanceName, true)

	// Convert to models
	var modelNodes []models.Node
	for _, node := range nodes {
		modelNode := models.Node{
			ID:       instanceName + "-" + node.Node,
			Name:     node.Node,
			Instance: instanceName,
			Host:     instanceCfg.Host, // Add the actual host URL
			Status:   node.Status,
			Type:     "node",
			CPU:      safeFloat(node.CPU), // Already in percentage
			Memory: models.Memory{
				Total: int64(node.MaxMem),
				Used:  int64(node.Mem),
				Free:  int64(node.MaxMem - node.Mem),
				Usage: safePercentage(float64(node.Mem), float64(node.MaxMem)),
			},
			Disk: models.Disk{
				Total: int64(node.MaxDisk),
				Used:  int64(node.Disk),
				Free:  int64(node.MaxDisk - node.Disk),
				Usage: safePercentage(float64(node.Disk), float64(node.MaxDisk)),
			},
			Uptime:           int64(node.Uptime),
			LoadAverage:      []float64{},
			LastSeen:         time.Now(),
			ConnectionHealth: "healthy",
		}

		// Debug logging for disk metrics - note that these values can fluctuate
		// due to thin provisioning and dynamic allocation
		log.Debug().
			Str("node", node.Node).
			Uint64("disk", node.Disk).
			Uint64("maxDisk", node.MaxDisk).
			Float64("diskUsage", safePercentage(float64(node.Disk), float64(node.MaxDisk))).
			Msg("Node disk metrics (raw from Proxmox)")

		// Get detailed node info if available
		nodeInfo, nodeErr := client.GetNodeStatus(ctx, node.Node)
		if nodeErr != nil {
			// If we can't get node status, it might be offline
			log.Debug().
				Str("instance", instanceName).
				Str("node", node.Node).
				Err(nodeErr).
				Msg("Could not get node status - node may be offline")
			// Continue with basic info we have
		} else if nodeInfo != nil {
			// Convert LoadAvg from interface{} to float64
			loadAvg := make([]float64, 0, len(nodeInfo.LoadAvg))
			for _, val := range nodeInfo.LoadAvg {
				switch v := val.(type) {
				case float64:
					loadAvg = append(loadAvg, v)
				case string:
					if f, err := strconv.ParseFloat(v, 64); err == nil {
						loadAvg = append(loadAvg, f)
					}
				}
			}
			modelNode.LoadAverage = loadAvg
			modelNode.KernelVersion = nodeInfo.KernelVersion
			modelNode.PVEVersion = nodeInfo.PVEVersion

			// Use rootfs data if available for more stable disk metrics
			if nodeInfo.RootFS != nil && nodeInfo.RootFS.Total > 0 {
				modelNode.Disk = models.Disk{
					Total: int64(nodeInfo.RootFS.Total),
					Used:  int64(nodeInfo.RootFS.Used),
					Free:  int64(nodeInfo.RootFS.Free),
					Usage: safePercentage(float64(nodeInfo.RootFS.Used), float64(nodeInfo.RootFS.Total)),
				}
				log.Debug().
					Str("node", node.Node).
					Uint64("rootfsUsed", nodeInfo.RootFS.Used).
					Uint64("rootfsTotal", nodeInfo.RootFS.Total).
					Float64("rootfsUsage", modelNode.Disk.Usage).
					Msg("Using rootfs for disk metrics")
			}

			if nodeInfo.CPUInfo != nil {
				// Use MaxCPU from node data for logical CPU count (includes hyperthreading)
				// If MaxCPU is not available or 0, fall back to physical cores
				logicalCores := node.MaxCPU
				if logicalCores == 0 {
					logicalCores = nodeInfo.CPUInfo.Cores
				}

				mhzStr := nodeInfo.CPUInfo.GetMHzString()
				log.Debug().
					Str("node", node.Node).
					Str("model", nodeInfo.CPUInfo.Model).
					Int("cores", nodeInfo.CPUInfo.Cores).
					Int("logicalCores", logicalCores).
					Int("sockets", nodeInfo.CPUInfo.Sockets).
					Str("mhz", mhzStr).
					Msg("Node CPU info from Proxmox")
				modelNode.CPUInfo = models.CPUInfo{
					Model:   nodeInfo.CPUInfo.Model,
					Cores:   logicalCores, // Use logical cores for display
					Sockets: nodeInfo.CPUInfo.Sockets,
					MHz:     mhzStr,
				}
			}
		} else {
			log.Debug().Err(err).Str("node", node.Node).Msg("Failed to get node status")
		}

		modelNodes = append(modelNodes, modelNode)
	}

	// Update state first so we have nodes available
	m.state.UpdateNodesForInstance(instanceName, modelNodes)

	// Now get storage data to use as fallback for disk metrics if needed
	storageByNode := make(map[string]models.Disk)
	if instanceCfg.MonitorStorage {
		_, err := client.GetAllStorage(ctx)
		if err == nil {
			for _, node := range nodes {
				nodeStorages, err := client.GetStorage(ctx, node.Node)
				if err == nil {
					// Look for local or local-lvm storage as most stable disk metric
					for _, storage := range nodeStorages {
						if storage.Storage == "local" || storage.Storage == "local-lvm" {
							disk := models.Disk{
								Total: int64(storage.Total),
								Used:  int64(storage.Used),
								Free:  int64(storage.Available),
								Usage: safePercentage(float64(storage.Used), float64(storage.Total)),
							}
							// Prefer "local" over "local-lvm"
							if _, exists := storageByNode[node.Node]; !exists || storage.Storage == "local" {
								storageByNode[node.Node] = disk
								log.Debug().
									Str("node", node.Node).
									Str("storage", storage.Storage).
									Float64("usage", disk.Usage).
									Msg("Using storage for disk metrics fallback")
							}
						}
					}
				}
			}
		}
	}

	// Update nodes with storage fallback if rootfs was not available
	for i := range modelNodes {
		if modelNodes[i].Disk.Total == 0 {
			if disk, exists := storageByNode[modelNodes[i].Name]; exists {
				modelNodes[i].Disk = disk
				log.Debug().
					Str("node", modelNodes[i].Name).
					Float64("usage", disk.Usage).
					Msg("Applied storage fallback for disk metrics")
			}
		}

		// Record node metrics history
		now := time.Now()
		m.metricsHistory.AddNodeMetric(modelNodes[i].ID, "cpu", modelNodes[i].CPU*100, now)
		m.metricsHistory.AddNodeMetric(modelNodes[i].ID, "memory", modelNodes[i].Memory.Usage, now)
		m.metricsHistory.AddNodeMetric(modelNodes[i].ID, "disk", modelNodes[i].Disk.Usage, now)

		// Check thresholds for alerts
		m.alertManager.CheckNode(modelNodes[i])
	}

	// Update state again with corrected disk metrics
	m.state.UpdateNodesForInstance(instanceName, modelNodes)

	// Update cluster endpoint online status if this is a cluster
	if instanceCfg.IsCluster && len(instanceCfg.ClusterEndpoints) > 0 {
		// Create a map of online nodes from our polling results
		onlineNodes := make(map[string]bool)
		for _, node := range modelNodes {
			// Node is online if we successfully got its data
			onlineNodes[node.Name] = node.Status == "online"
		}

		// Update the online status for each cluster endpoint
		for i := range instanceCfg.ClusterEndpoints {
			if online, exists := onlineNodes[instanceCfg.ClusterEndpoints[i].NodeName]; exists {
				instanceCfg.ClusterEndpoints[i].Online = online
				if online {
					instanceCfg.ClusterEndpoints[i].LastSeen = time.Now()
				}
			}
		}

		// Update the config with the new online status
		// This is needed so the UI can reflect the current status
		for idx, cfg := range m.config.PVEInstances {
			if cfg.Name == instanceName {
				m.config.PVEInstances[idx].ClusterEndpoints = instanceCfg.ClusterEndpoints
				break
			}
		}
	}

	// Poll VMs and containers together using cluster/resources for efficiency
	if instanceCfg.MonitorVMs || instanceCfg.MonitorContainers {
		select {
		case <-ctx.Done():
			return
		default:
			// Only try cluster endpoints if this is configured as a cluster
			// This prevents syslog spam on non-clustered nodes from certificate checks
			useClusterEndpoint := false
			if instanceCfg.IsCluster {
				// Double-check that this is actually a cluster to prevent misconfiguration
				// This helps avoid certificate spam on standalone nodes incorrectly marked as clusters
				isActuallyCluster, _ := client.IsClusterMember(ctx)
				if isActuallyCluster {
					// Try to use efficient cluster/resources endpoint
					useClusterEndpoint = m.pollVMsAndContainersEfficient(ctx, instanceName, client)
				} else {
					// Misconfigured - marked as cluster but isn't one
					log.Warn().
						Str("instance", instanceName).
						Msg("Instance marked as cluster but is actually standalone - consider updating configuration")
					instanceCfg.IsCluster = false
				}
			}

			if !useClusterEndpoint {
				// Use traditional polling for non-clusters or if cluster endpoint fails
				// Use WithNodes versions to avoid duplicate GetNodes calls
				if instanceCfg.MonitorVMs {
					m.pollVMsWithNodes(ctx, instanceName, client, nodes)
				}
				if instanceCfg.MonitorContainers {
					m.pollContainersWithNodes(ctx, instanceName, client, nodes)
				}
			}
		}
	}

	// Poll storage if enabled
	if instanceCfg.MonitorStorage {
		select {
		case <-ctx.Done():
			return
		default:
			m.pollStorageWithNodes(ctx, instanceName, client, nodes)
		}
	}

	// Poll backups if enabled - using configurable cycle count
	// This prevents slow backup/snapshot queries from blocking real-time stats
	// Also poll on first cycle (pollCounter == 1) to ensure data loads quickly
	backupCycles := 10 // default
	if m.config.BackupPollingCycles > 0 {
		backupCycles = m.config.BackupPollingCycles
	}
	if instanceCfg.MonitorBackups && (m.pollCounter%int64(backupCycles) == 0 || m.pollCounter == 1) {
		select {
		case <-ctx.Done():
			return
		default:
			// Run backup polling in a separate goroutine to not block main polling
			go func() {
				log.Info().Str("instance", instanceName).Msg("Starting background backup/snapshot polling")
				// Create a separate context with longer timeout for backup operations
				backupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()

				// Poll backup tasks
				m.pollBackupTasks(backupCtx, instanceName, client)

				// Poll storage backups - pass nodes to avoid duplicate API calls
				m.pollStorageBackupsWithNodes(backupCtx, instanceName, client, nodes)

				// Poll guest snapshots
				m.pollGuestSnapshots(backupCtx, instanceName, client)

				log.Info().Str("instance", instanceName).Msg("Completed background backup/snapshot polling")
			}()
		}
	}
}

// pollVMsAndContainersEfficient uses the cluster/resources endpoint to get all VMs and containers in one call
// This should only be called for instances configured as clusters
func (m *Monitor) pollVMsAndContainersEfficient(ctx context.Context, instanceName string, client PVEClientInterface) bool {
	log.Info().Str("instance", instanceName).Msg("Polling VMs and containers using cluster/resources")

	// Get all resources in a single API call
	resources, err := client.GetClusterResources(ctx, "vm")
	if err != nil {
		log.Debug().Err(err).Str("instance", instanceName).Msg("cluster/resources not available, falling back to traditional polling")
		return false
	}

	var allVMs []models.VM
	var allContainers []models.Container

	for _, res := range resources {
		guestID := fmt.Sprintf("%s-%s-%d", instanceName, res.Node, res.VMID)

		// Debug log the resource type
		log.Debug().
			Str("instance", instanceName).
			Str("name", res.Name).
			Int("vmid", res.VMID).
			Str("type", res.Type).
			Msg("Processing cluster resource")

		// Calculate I/O rates
		currentMetrics := IOMetrics{
			DiskRead:   int64(res.DiskRead),
			DiskWrite:  int64(res.DiskWrite),
			NetworkIn:  int64(res.NetIn),
			NetworkOut: int64(res.NetOut),
			Timestamp:  time.Now(),
		}
		diskReadRate, diskWriteRate, netInRate, netOutRate := m.rateTracker.CalculateRates(guestID, currentMetrics)

		if res.Type == "qemu" {
			// Skip templates if configured
			if res.Template == 1 {
				continue
			}

			vm := models.VM{
				ID:       guestID,
				VMID:     res.VMID,
				Name:     res.Name,
				Node:     res.Node,
				Instance: instanceName,
				Status:   res.Status,
				Type:     "qemu",
				CPU:      safeFloat(res.CPU),
				CPUs:     res.MaxCPU,
				Memory: models.Memory{
					Total: int64(res.MaxMem),
					Used:  int64(res.Mem),
					Free:  int64(res.MaxMem - res.Mem),
					Usage: safePercentage(float64(res.Mem), float64(res.MaxMem)),
				},
				Disk: models.Disk{
					Total: int64(res.MaxDisk),
					Used:  int64(res.Disk),
					Free:  int64(res.MaxDisk - res.Disk),
					Usage: safePercentage(float64(res.Disk), float64(res.MaxDisk)),
				},
				NetworkIn:  maxInt64(0, int64(netInRate)),
				NetworkOut: maxInt64(0, int64(netOutRate)),
				DiskRead:   maxInt64(0, int64(diskReadRate)),
				DiskWrite:  maxInt64(0, int64(diskWriteRate)),
				Uptime:     int64(res.Uptime),
				Template:   res.Template == 1,
				LastSeen:   time.Now(),
			}

			// Parse tags
			if res.Tags != "" {
				vm.Tags = strings.Split(res.Tags, ";")
			}

			allVMs = append(allVMs, vm)

			// Check thresholds for alerts
			m.alertManager.CheckGuest(vm, instanceName)

		} else if res.Type == "lxc" {
			// Skip templates if configured
			if res.Template == 1 {
				continue
			}

			container := models.Container{
				ID:       guestID,
				VMID:     res.VMID,
				Name:     res.Name,
				Node:     res.Node,
				Instance: instanceName,
				Status:   res.Status,
				Type:     "lxc",
				CPU:      safeFloat(res.CPU),
				CPUs:     int(res.MaxCPU),
				Memory: models.Memory{
					Total: int64(res.MaxMem),
					Used:  int64(res.Mem),
					Free:  int64(res.MaxMem - res.Mem),
					Usage: safePercentage(float64(res.Mem), float64(res.MaxMem)),
				},
				Disk: models.Disk{
					Total: int64(res.MaxDisk),
					Used:  int64(res.Disk),
					Free:  int64(res.MaxDisk - res.Disk),
					Usage: safePercentage(float64(res.Disk), float64(res.MaxDisk)),
				},
				NetworkIn:  maxInt64(0, int64(netInRate)),
				NetworkOut: maxInt64(0, int64(netOutRate)),
				DiskRead:   maxInt64(0, int64(diskReadRate)),
				DiskWrite:  maxInt64(0, int64(diskWriteRate)),
				Uptime:     int64(res.Uptime),
				Template:   res.Template == 1,
				LastSeen:   time.Now(),
			}

			// Parse tags
			if res.Tags != "" {
				container.Tags = strings.Split(res.Tags, ";")
			}

			allContainers = append(allContainers, container)

			// Check thresholds for alerts
			m.alertManager.CheckGuest(container, instanceName)
		}
	}

	// Update state
	if len(allVMs) > 0 {
		m.state.UpdateVMsForInstance(instanceName, allVMs)
	}
	if len(allContainers) > 0 {
		m.state.UpdateContainersForInstance(instanceName, allContainers)
	}

	log.Info().
		Str("instance", instanceName).
		Int("vms", len(allVMs)).
		Int("containers", len(allContainers)).
		Msg("VMs and containers polled efficiently with cluster/resources")

	return true
}

// pollVMs polls VMs from a PVE instance
// Deprecated: This function should not be called directly as it causes duplicate GetNodes calls.
// Use pollVMsWithNodes instead.
func (m *Monitor) pollVMs(ctx context.Context, instanceName string, client PVEClientInterface) {
	log.Warn().Str("instance", instanceName).Msg("pollVMs called directly - this causes duplicate GetNodes calls and syslog spam on non-clustered nodes")

	// Get all nodes first
	nodes, err := client.GetNodes(ctx)
	if err != nil {
		monErr := errors.WrapConnectionError("get_nodes_for_vms", instanceName, err)
		log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to get nodes for VM polling")
		return
	}

	m.pollVMsWithNodes(ctx, instanceName, client, nodes)
}

// pollVMsWithNodes polls VMs using a provided nodes list to avoid duplicate GetNodes calls
func (m *Monitor) pollVMsWithNodes(ctx context.Context, instanceName string, client PVEClientInterface, nodes []proxmox.Node) {
	var allVMs []models.VM
	for _, node := range nodes {
		vms, err := client.GetVMs(ctx, node.Node)
		if err != nil {
			monErr := errors.NewMonitorError(errors.ErrorTypeAPI, "get_vms", instanceName, err).WithNode(node.Node)
			log.Error().Err(monErr).Str("node", node.Node).Msg("Failed to get VMs")
			continue
		}

		for _, vm := range vms {
			// Skip templates if configured
			if vm.Template == 1 {
				continue
			}

			// Parse tags
			var tags []string
			if vm.Tags != "" {
				tags = strings.Split(vm.Tags, ";")
			}

			// Calculate I/O rates
			guestID := fmt.Sprintf("%s-%s-%d", instanceName, node.Node, vm.VMID)
			currentMetrics := IOMetrics{
				DiskRead:   int64(vm.DiskRead),
				DiskWrite:  int64(vm.DiskWrite),
				NetworkIn:  int64(vm.NetIn),
				NetworkOut: int64(vm.NetOut),
				Timestamp:  time.Now(),
			}
			diskReadRate, diskWriteRate, netInRate, netOutRate := m.rateTracker.CalculateRates(guestID, currentMetrics)

			// For running VMs, try to get detailed status with balloon info
			memUsed := uint64(0)
			memTotal := vm.MaxMem

			if vm.Status == "running" {
				// Try to get detailed VM status for more accurate memory reporting
				if vmStatus, err := client.GetVMStatus(ctx, node.Node, vm.VMID); err == nil {
					// If balloon is enabled, use balloon as the total available memory
					if vmStatus.Balloon > 0 && vmStatus.Balloon < vmStatus.MaxMem {
						memTotal = vmStatus.Balloon
					}

					// If we have free memory from guest agent, calculate actual usage
					if vmStatus.FreeMem > 0 {
						// Guest agent reports free memory, so calculate used
						memUsed = memTotal - vmStatus.FreeMem
					} else if vmStatus.Mem > 0 {
						// No guest agent free memory data, but we have actual memory usage
						// Use the reported memory usage from Proxmox
						memUsed = vmStatus.Mem
					} else {
						// No memory data available at all - show 0% usage
						memUsed = 0
					}
				} else {
					// Failed to get detailed status - show 0% usage
					memUsed = 0
				}
			} else {
				// VM is not running, show 0 usage
				memUsed = 0
			}

			// Set CPU to 0 for non-running VMs to avoid false alerts
			// VMs can have status: running, stopped, paused, suspended
			cpuUsage := safeFloat(vm.CPU)
			if vm.Status != "running" {
				cpuUsage = 0
			}

			modelVM := models.VM{
				ID:       guestID,
				VMID:     vm.VMID,
				Name:     vm.Name,
				Node:     node.Node,
				Instance: instanceName,
				Status:   vm.Status,
				Type:     "qemu",
				CPU:      cpuUsage, // Already in percentage
				CPUs:     vm.CPUs,
				Memory: models.Memory{
					Total: int64(memTotal),
					Used:  int64(memUsed),
					Free:  int64(memTotal - memUsed),
					Usage: safePercentage(float64(memUsed), float64(memTotal)),
				},
				Disk: models.Disk{
					Total: int64(vm.MaxDisk),
					Used:  int64(vm.Disk),
					Free:  int64(vm.MaxDisk - vm.Disk),
					Usage: safePercentage(float64(vm.Disk), float64(vm.MaxDisk)),
				},
				NetworkIn:  maxInt64(0, int64(netInRate)),
				NetworkOut: maxInt64(0, int64(netOutRate)),
				DiskRead:   maxInt64(0, int64(diskReadRate)),
				DiskWrite:  maxInt64(0, int64(diskWriteRate)),
				Uptime:     int64(vm.Uptime),
				Template:   vm.Template == 1,
				Tags:       tags,
				Lock:       vm.Lock,
				LastSeen:   time.Now(),
			}
			allVMs = append(allVMs, modelVM)

			// Record metrics history
			now := time.Now()
			m.metricsHistory.AddGuestMetric(modelVM.ID, "cpu", modelVM.CPU*100, now)
			m.metricsHistory.AddGuestMetric(modelVM.ID, "memory", modelVM.Memory.Usage, now)
			m.metricsHistory.AddGuestMetric(modelVM.ID, "disk", modelVM.Disk.Usage, now)
			m.metricsHistory.AddGuestMetric(modelVM.ID, "diskread", float64(modelVM.DiskRead), now)
			m.metricsHistory.AddGuestMetric(modelVM.ID, "diskwrite", float64(modelVM.DiskWrite), now)
			m.metricsHistory.AddGuestMetric(modelVM.ID, "netin", float64(modelVM.NetworkIn), now)
			m.metricsHistory.AddGuestMetric(modelVM.ID, "netout", float64(modelVM.NetworkOut), now)

			// Check thresholds for alerts
			m.alertManager.CheckGuest(modelVM, instanceName)
		}
	}

	m.state.UpdateVMsForInstance(instanceName, allVMs)
}

// pollContainers polls containers from a PVE instance
// Deprecated: This function should not be called directly as it causes duplicate GetNodes calls.
// Use pollContainersWithNodes instead.
func (m *Monitor) pollContainers(ctx context.Context, instanceName string, client PVEClientInterface) {
	log.Warn().Str("instance", instanceName).Msg("pollContainers called directly - this causes duplicate GetNodes calls and syslog spam on non-clustered nodes")

	// Get all nodes first
	nodes, err := client.GetNodes(ctx)
	if err != nil {
		monErr := errors.WrapConnectionError("get_nodes_for_containers", instanceName, err)
		log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to get nodes for container polling")
		return
	}

	m.pollContainersWithNodes(ctx, instanceName, client, nodes)
}

// pollContainersWithNodes polls containers using a provided nodes list to avoid duplicate GetNodes calls
func (m *Monitor) pollContainersWithNodes(ctx context.Context, instanceName string, client PVEClientInterface, nodes []proxmox.Node) {

	var allContainers []models.Container
	for _, node := range nodes {
		containers, err := client.GetContainers(ctx, node.Node)
		if err != nil {
			monErr := errors.NewMonitorError(errors.ErrorTypeAPI, "get_containers", instanceName, err).WithNode(node.Node)
			log.Error().Err(monErr).Str("node", node.Node).Msg("Failed to get containers")
			continue
		}

		for _, ct := range containers {
			// Skip templates if configured
			if ct.Template == 1 {
				continue
			}

			// Parse tags
			var tags []string
			if ct.Tags != "" {
				tags = strings.Split(ct.Tags, ";")
			}

			// Calculate I/O rates
			guestID := fmt.Sprintf("%s-%s-%d", instanceName, node.Node, ct.VMID)
			currentMetrics := IOMetrics{
				DiskRead:   int64(ct.DiskRead),
				DiskWrite:  int64(ct.DiskWrite),
				NetworkIn:  int64(ct.NetIn),
				NetworkOut: int64(ct.NetOut),
				Timestamp:  time.Now(),
			}
			diskReadRate, diskWriteRate, netInRate, netOutRate := m.rateTracker.CalculateRates(guestID, currentMetrics)

			// Set CPU to 0 for non-running containers to avoid false alerts
			// Containers can have status: running, stopped, paused, suspended
			cpuUsage := safeFloat(ct.CPU)
			if ct.Status != "running" {
				cpuUsage = 0
			}

			// For containers, memory reporting is more accurate than VMs
			// ct.Mem shows actual usage for running containers
			memUsed := uint64(0)
			memTotal := ct.MaxMem

			if ct.Status == "running" {
				// For running containers, ct.Mem is actual usage
				memUsed = ct.Mem
			}

			// Convert -1 to nil for I/O metrics when VM is not running
			// We'll use -1 to indicate "no data" which will be converted to null for the frontend
			modelCT := models.Container{
				ID:       guestID,
				VMID:     ct.VMID,
				Name:     ct.Name,
				Node:     node.Node,
				Instance: instanceName,
				Status:   ct.Status,
				Type:     "lxc",
				CPU:      cpuUsage, // Already in percentage
				CPUs:     int(ct.CPUs),
				Memory: models.Memory{
					Total: int64(memTotal),
					Used:  int64(memUsed),
					Free:  int64(memTotal - memUsed),
					Usage: safePercentage(float64(memUsed), float64(memTotal)),
				},
				Disk: models.Disk{
					Total: int64(ct.MaxDisk),
					Used:  int64(ct.Disk),
					Free:  int64(ct.MaxDisk - ct.Disk),
					Usage: safePercentage(float64(ct.Disk), float64(ct.MaxDisk)),
				},
				NetworkIn:  maxInt64(0, int64(netInRate)),
				NetworkOut: maxInt64(0, int64(netOutRate)),
				DiskRead:   maxInt64(0, int64(diskReadRate)),
				DiskWrite:  maxInt64(0, int64(diskWriteRate)),
				Uptime:     int64(ct.Uptime),
				Template:   ct.Template == 1,
				Tags:       tags,
				Lock:       ct.Lock,
				LastSeen:   time.Now(),
			}
			allContainers = append(allContainers, modelCT)

			// Record metrics history
			now := time.Now()
			m.metricsHistory.AddGuestMetric(modelCT.ID, "cpu", modelCT.CPU*100, now)
			m.metricsHistory.AddGuestMetric(modelCT.ID, "memory", modelCT.Memory.Usage, now)
			m.metricsHistory.AddGuestMetric(modelCT.ID, "disk", modelCT.Disk.Usage, now)
			m.metricsHistory.AddGuestMetric(modelCT.ID, "diskread", float64(modelCT.DiskRead), now)
			m.metricsHistory.AddGuestMetric(modelCT.ID, "diskwrite", float64(modelCT.DiskWrite), now)
			m.metricsHistory.AddGuestMetric(modelCT.ID, "netin", float64(modelCT.NetworkIn), now)
			m.metricsHistory.AddGuestMetric(modelCT.ID, "netout", float64(modelCT.NetworkOut), now)

			// Check thresholds for alerts
			log.Info().Str("container", modelCT.Name).Msg("Checking container alerts")
			m.alertManager.CheckGuest(modelCT, instanceName)
		}
	}

	m.state.UpdateContainersForInstance(instanceName, allContainers)
}

// pollStorage polls storage from a PVE instance
// Deprecated: This function should not be called directly as it causes duplicate GetNodes calls.
// Use pollStorageWithNodes instead.
func (m *Monitor) pollStorage(ctx context.Context, instanceName string, client PVEClientInterface) {
	log.Warn().Str("instance", instanceName).Msg("pollStorage called directly - this causes duplicate GetNodes calls and syslog spam on non-clustered nodes")

	// Get all nodes first
	nodes, err := client.GetNodes(ctx)
	if err != nil {
		monErr := errors.WrapConnectionError("get_nodes_for_storage", instanceName, err)
		log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to get nodes for storage polling")
		return
	}

	m.pollStorageWithNodes(ctx, instanceName, client, nodes)
}

// pollStorageWithNodes polls storage using a provided nodes list to avoid duplicate GetNodes calls
func (m *Monitor) pollStorageWithNodes(ctx context.Context, instanceName string, client PVEClientInterface, nodes []proxmox.Node) {

	// Get cluster storage configuration for shared/enabled status
	clusterStorages, err := client.GetAllStorage(ctx)
	if err != nil {
		monErr := errors.WrapAPIError("get_cluster_storage", instanceName, err, 0)
		log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to get cluster storage")
	}

	// Create a map for quick lookup of cluster storage config
	clusterStorageMap := make(map[string]proxmox.Storage)
	for _, cs := range clusterStorages {
		clusterStorageMap[cs.Storage] = cs
	}

	var allStorage []models.Storage
	seenStorage := make(map[string]bool)

	// Get storage from each node (this includes capacity info)
	for _, node := range nodes {
		nodeStorage, err := client.GetStorage(ctx, node.Node)
		if err != nil {
			monErr := errors.NewMonitorError(errors.ErrorTypeAPI, "get_node_storage", instanceName, err).WithNode(node.Node)
			log.Error().Err(monErr).Str("node", node.Node).Msg("Failed to get node storage")
			continue
		}

		for _, storage := range nodeStorage {
			// Get cluster config for this storage
			clusterConfig, hasClusterConfig := clusterStorageMap[storage.Storage]

			// Determine if shared
			shared := hasClusterConfig && clusterConfig.Shared == 1

			// For shared storage, only include it once
			storageKey := storage.Storage
			if shared {
				if seenStorage[storageKey] {
					continue
				}
				seenStorage[storageKey] = true
			}

			// Use appropriate node name
			nodeID := node.Node
			storageID := fmt.Sprintf("%s-%s-%s", instanceName, nodeID, storage.Storage)
			if shared {
				nodeID = "shared"
				// Use a consistent ID for shared storage across all instances
				storageID = fmt.Sprintf("shared-%s", storage.Storage)
			}

			modelStorage := models.Storage{
				ID:       storageID,
				Name:     storage.Storage,
				Node:     nodeID,
				Instance: instanceName,
				Type:     storage.Type,
				Status:   "available",
				Total:    int64(storage.Total),
				Used:     int64(storage.Used),
				Free:     int64(storage.Available),
				Usage:    0,
				Content:  sortContent(storage.Content),
				Shared:   shared,
				Enabled:  true,
				Active:   true,
			}

			// Override with cluster config if available
			if hasClusterConfig {
				// Sort content values for consistent display
				if clusterConfig.Content != "" {
					contentParts := strings.Split(clusterConfig.Content, ",")
					sort.Strings(contentParts)
					modelStorage.Content = strings.Join(contentParts, ",")
				} else {
					modelStorage.Content = clusterConfig.Content
				}
				modelStorage.Enabled = clusterConfig.Enabled == 1
				modelStorage.Active = clusterConfig.Active == 1
			}

			// Calculate usage percentage
			if modelStorage.Total > 0 {
				modelStorage.Usage = safePercentage(float64(modelStorage.Used), float64(modelStorage.Total))
			}

			// Determine status based on active/enabled flags
			if storage.Active == 1 || modelStorage.Active {
				modelStorage.Status = "available"
			} else if modelStorage.Enabled {
				modelStorage.Status = "inactive"
			} else {
				modelStorage.Status = "disabled"
			}

			allStorage = append(allStorage, modelStorage)

			// Record storage metrics history
			now := time.Now()
			m.metricsHistory.AddStorageMetric(modelStorage.ID, "usage", modelStorage.Usage, now)
			m.metricsHistory.AddStorageMetric(modelStorage.ID, "used", float64(modelStorage.Used), now)
			m.metricsHistory.AddStorageMetric(modelStorage.ID, "total", float64(modelStorage.Total), now)
			m.metricsHistory.AddStorageMetric(modelStorage.ID, "avail", float64(modelStorage.Free), now)

			// Check thresholds for alerts
			m.alertManager.CheckStorage(modelStorage)
		}
	}

	// Update storage for this instance only
	var instanceStorage []models.Storage
	for _, st := range allStorage {
		st.Instance = instanceName
		instanceStorage = append(instanceStorage, st)
	}
	m.state.UpdateStorageForInstance(instanceName, instanceStorage)
}

// pollBackupTasks polls backup tasks from a PVE instance
func (m *Monitor) pollBackupTasks(ctx context.Context, instanceName string, client PVEClientInterface) {
	log.Debug().Str("instance", instanceName).Msg("Polling backup tasks")

	tasks, err := client.GetBackupTasks(ctx)
	if err != nil {
		monErr := errors.WrapAPIError("get_backup_tasks", instanceName, err, 0)
		log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to get backup tasks")
		return
	}

	var backupTasks []models.BackupTask
	for _, task := range tasks {
		// Extract VMID from task ID (format: "UPID:node:pid:starttime:type:vmid:user@realm:")
		vmid := 0
		if task.ID != "" {
			if vmidInt, err := strconv.Atoi(task.ID); err == nil {
				vmid = vmidInt
			}
		}

		taskID := fmt.Sprintf("%s-%s", instanceName, task.UPID)

		backupTask := models.BackupTask{
			ID:        taskID,
			Node:      task.Node,
			Type:      task.Type,
			VMID:      vmid,
			Status:    task.Status,
			StartTime: time.Unix(task.StartTime, 0),
		}

		if task.EndTime > 0 {
			backupTask.EndTime = time.Unix(task.EndTime, 0)
		}

		backupTasks = append(backupTasks, backupTask)
	}

	// Update state with new backup tasks for this instance
	m.state.UpdateBackupTasksForInstance(instanceName, backupTasks)
}

// pollPBSInstance polls a single PBS instance
func (m *Monitor) pollPBSInstance(ctx context.Context, instanceName string, client *pbs.Client) {
	// Check if context is cancelled
	select {
	case <-ctx.Done():
		log.Debug().Str("instance", instanceName).Msg("Polling cancelled")
		return
	default:
	}

	log.Debug().Str("instance", instanceName).Msg("Polling PBS instance")

	// Get instance config
	var instanceCfg *config.PBSInstance
	for _, cfg := range m.config.PBSInstances {
		if cfg.Name == instanceName {
			instanceCfg = &cfg
			log.Debug().
				Str("instance", instanceName).
				Bool("monitorDatastores", cfg.MonitorDatastores).
				Msg("Found PBS instance config")
			break
		}
	}
	if instanceCfg == nil {
		log.Error().Str("instance", instanceName).Msg("PBS instance config not found")
		return
	}

	// Initialize PBS instance with default values
	pbsInst := models.PBSInstance{
		ID:               "pbs-" + instanceName,
		Name:             instanceName,
		Host:             instanceCfg.Host,
		Status:           "offline",
		Version:          "unknown",
		ConnectionHealth: "unhealthy",
		LastSeen:         time.Now(),
	}

	// Try to get version first
	version, versionErr := client.GetVersion(ctx)
	if versionErr == nil {
		// Version succeeded - PBS is online
		pbsInst.Status = "online"
		pbsInst.Version = version.Version
		pbsInst.ConnectionHealth = "healthy"
		m.resetAuthFailures(instanceName, "pbs")
		m.state.SetConnectionHealth("pbs-"+instanceName, true)

		log.Debug().
			Str("instance", instanceName).
			Str("version", version.Version).
			Bool("monitorDatastores", instanceCfg.MonitorDatastores).
			Msg("PBS version retrieved successfully")
	} else {
		log.Debug().Err(versionErr).Str("instance", instanceName).Msg("Failed to get PBS version, trying fallback")

		// Version failed, try datastores as fallback (like test connection does)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()

		_, datastoreErr := client.GetDatastores(ctx2)
		if datastoreErr == nil {
			// Datastores succeeded - PBS is online but version unavailable
			pbsInst.Status = "online"
			pbsInst.Version = "connected"
			pbsInst.ConnectionHealth = "healthy"
			m.resetAuthFailures(instanceName, "pbs")
			m.state.SetConnectionHealth("pbs-"+instanceName, true)

			log.Info().
				Str("instance", instanceName).
				Msg("PBS connected (version unavailable but datastores accessible)")
		} else {
			// Both failed - PBS is offline
			pbsInst.Status = "offline"
			pbsInst.ConnectionHealth = "error"
			monErr := errors.WrapConnectionError("get_pbs_version", instanceName, versionErr)
			log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to connect to PBS")
			m.state.SetConnectionHealth("pbs-"+instanceName, false)

			// Track auth failure if it's an authentication error
			if errors.IsAuthError(versionErr) || errors.IsAuthError(datastoreErr) {
				m.recordAuthFailure(instanceName, "pbs")
				// Don't continue if auth failed
				return
			}
		}
	}

	// Get node status (CPU, memory, etc.)
	// Note: This requires Sys.Audit permission on PBS which read-only tokens often don't have
	nodeStatus, err := client.GetNodeStatus(ctx)
	if err != nil {
		// Log as debug instead of error since this is often a permission issue
		log.Debug().Err(err).Str("instance", instanceName).Msg("Could not get PBS node status (may need Sys.Audit permission)")
	} else {
		pbsInst.CPU = nodeStatus.CPU
		if nodeStatus.Memory.Total > 0 {
			pbsInst.Memory = float64(nodeStatus.Memory.Used) / float64(nodeStatus.Memory.Total) * 100
			pbsInst.MemoryUsed = nodeStatus.Memory.Used
			pbsInst.MemoryTotal = nodeStatus.Memory.Total
		}
		pbsInst.Uptime = nodeStatus.Uptime

		log.Debug().
			Str("instance", instanceName).
			Float64("cpu", pbsInst.CPU).
			Float64("memory", pbsInst.Memory).
			Int64("uptime", pbsInst.Uptime).
			Msg("PBS node status retrieved")
	}

	// Poll datastores if enabled
	log.Debug().Bool("monitorDatastores", instanceCfg.MonitorDatastores).Str("instance", instanceName).Msg("Checking if datastore monitoring is enabled")
	if instanceCfg.MonitorDatastores {
		datastores, err := client.GetDatastores(ctx)
		if err != nil {
			monErr := errors.WrapAPIError("get_datastores", instanceName, err, 0)
			log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to get datastores")
		} else {
			log.Info().
				Str("instance", instanceName).
				Int("count", len(datastores)).
				Msg("Got PBS datastores")

			for _, ds := range datastores {
				// Use whichever fields are populated
				total := ds.Total
				if total == 0 && ds.TotalSpace > 0 {
					total = ds.TotalSpace
				}
				used := ds.Used
				if used == 0 && ds.UsedSpace > 0 {
					used = ds.UsedSpace
				}
				avail := ds.Avail
				if avail == 0 && ds.AvailSpace > 0 {
					avail = ds.AvailSpace
				}

				// If still 0, try to calculate from each other
				if total == 0 && used > 0 && avail > 0 {
					total = used + avail
				}

				log.Debug().
					Str("store", ds.Store).
					Int64("total", total).
					Int64("used", used).
					Int64("avail", avail).
					Int64("orig_total", ds.Total).
					Int64("orig_total_space", ds.TotalSpace).
					Msg("PBS datastore details")

				modelDS := models.PBSDatastore{
					Name:   ds.Store,
					Total:  total,
					Used:   used,
					Free:   avail,
					Usage:  safePercentage(float64(used), float64(total)),
					Status: "available",
				}

				// Discover namespaces for this datastore
				namespaces, err := client.ListNamespaces(ctx, ds.Store, "", 0)
				if err != nil {
					log.Warn().Err(err).
						Str("instance", instanceName).
						Str("datastore", ds.Store).
						Msg("Failed to list namespaces")
				} else {
					// Convert PBS namespaces to model namespaces
					for _, ns := range namespaces {
						nsPath := ns.NS
						if nsPath == "" {
							nsPath = ns.Path
						}
						if nsPath == "" {
							nsPath = ns.Name
						}

						modelNS := models.PBSNamespace{
							Path:   nsPath,
							Parent: ns.Parent,
							Depth:  strings.Count(nsPath, "/"),
						}
						modelDS.Namespaces = append(modelDS.Namespaces, modelNS)
					}

					// Always include root namespace
					hasRoot := false
					for _, ns := range modelDS.Namespaces {
						if ns.Path == "" {
							hasRoot = true
							break
						}
					}
					if !hasRoot {
						modelDS.Namespaces = append([]models.PBSNamespace{{Path: "", Depth: 0}}, modelDS.Namespaces...)
					}
				}

				pbsInst.Datastores = append(pbsInst.Datastores, modelDS)
			}
		}
	}

	// Update state - merge with existing instances
	m.state.UpdatePBSInstance(pbsInst)
	log.Info().
		Str("instance", instanceName).
		Str("id", pbsInst.ID).
		Int("datastores", len(pbsInst.Datastores)).
		Msg("PBS instance updated in state")

	// Poll backups if enabled
	if instanceCfg.MonitorBackups {
		log.Info().
			Str("instance", instanceName).
			Int("datastores", len(pbsInst.Datastores)).
			Msg("Polling PBS backups")
		m.pollPBSBackups(ctx, instanceName, client, pbsInst.Datastores)
	} else {
		log.Debug().
			Str("instance", instanceName).
			Msg("PBS backup monitoring disabled")
	}
}

// GetState returns the current state
func (m *Monitor) GetState() models.StateSnapshot {
	return m.state.GetSnapshot()
}

// GetStartTime returns the monitor start time
func (m *Monitor) GetStartTime() time.Time {
	return m.startTime
}

// GetDiscoveryService returns the discovery service
func (m *Monitor) GetDiscoveryService() *discovery.Service {
	return m.discoveryService
}

// GetGuestMetrics returns historical metrics for a guest
func (m *Monitor) GetGuestMetrics(guestID string, duration time.Duration) map[string][]MetricPoint {
	return m.metricsHistory.GetAllGuestMetrics(guestID, duration)
}

// GetNodeMetrics returns historical metrics for a node
func (m *Monitor) GetNodeMetrics(nodeID string, metricType string, duration time.Duration) []MetricPoint {
	return m.metricsHistory.GetNodeMetrics(nodeID, metricType, duration)
}

// GetStorageMetrics returns historical metrics for storage
func (m *Monitor) GetStorageMetrics(storageID string, duration time.Duration) map[string][]MetricPoint {
	return m.metricsHistory.GetAllStorageMetrics(storageID, duration)
}

// GetAlertManager returns the alert manager
func (m *Monitor) GetAlertManager() *alerts.Manager {
	return m.alertManager
}

// GetNotificationManager returns the notification manager
func (m *Monitor) GetNotificationManager() *notifications.NotificationManager {
	return m.notificationMgr
}

// GetConfigPersistence returns the config persistence manager
func (m *Monitor) GetConfigPersistence() *config.ConfigPersistence {
	return m.configPersist
}

// pollStorageBackups polls backup files from storage
// Deprecated: This function should not be called directly as it causes duplicate GetNodes calls.
// Use pollStorageBackupsWithNodes instead.
func (m *Monitor) pollStorageBackups(ctx context.Context, instanceName string, client PVEClientInterface) {
	log.Warn().Str("instance", instanceName).Msg("pollStorageBackups called directly - this causes duplicate GetNodes calls and syslog spam on non-clustered nodes")

	// Get all nodes
	nodes, err := client.GetNodes(ctx)
	if err != nil {
		monErr := errors.WrapConnectionError("get_nodes_for_backups", instanceName, err)
		log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to get nodes for backup polling")
		return
	}

	m.pollStorageBackupsWithNodes(ctx, instanceName, client, nodes)
}

// pollStorageBackupsWithNodes polls backups using a provided nodes list to avoid duplicate GetNodes calls
func (m *Monitor) pollStorageBackupsWithNodes(ctx context.Context, instanceName string, client PVEClientInterface, nodes []proxmox.Node) {

	var allBackups []models.StorageBackup
	seenVolids := make(map[string]bool) // Track seen volume IDs to avoid duplicates

	// For each node, get storage and check content
	for _, node := range nodes {
		if node.Status != "online" {
			continue
		}

		// Get storage for this node
		storages, err := client.GetStorage(ctx, node.Node)
		if err != nil {
			monErr := errors.NewMonitorError(errors.ErrorTypeAPI, "get_storage_for_backups", instanceName, err).WithNode(node.Node)
			log.Error().Err(monErr).Str("node", node.Node).Msg("Failed to get storage")
			continue
		}

		// For each storage that can contain backups or templates
		for _, storage := range storages {
			// Check if storage supports backup content
			if !strings.Contains(storage.Content, "backup") {
				continue
			}

			// Get storage content
			contents, err := client.GetStorageContent(ctx, node.Node, storage.Storage)
			if err != nil {
				monErr := errors.NewMonitorError(errors.ErrorTypeAPI, "get_storage_content", instanceName, err).WithNode(node.Node)
				log.Debug().Err(monErr).
					Str("node", node.Node).
					Str("storage", storage.Storage).
					Msg("Failed to get storage content")
				continue
			}

			// Convert to models
			for _, content := range contents {
				// Skip if we've already seen this item (shared storage duplicate)
				if seenVolids[content.Volid] {
					continue
				}
				seenVolids[content.Volid] = true

				// Skip templates and ISOs - they're not backups
				if content.Content == "vztmpl" || content.Content == "iso" {
					continue
				}

				// Determine type from content type and volid
				backupType := "unknown"
				if strings.Contains(content.Volid, "/vm/") || strings.Contains(content.Volid, "qemu") {
					backupType = "qemu"
				} else if strings.Contains(content.Volid, "/ct/") || strings.Contains(content.Volid, "lxc") {
					backupType = "lxc"
				} else if strings.Contains(content.Format, "pbs-ct") {
					// PBS format check as fallback
					backupType = "lxc"
				} else if strings.Contains(content.Format, "pbs-vm") {
					// PBS format check as fallback
					backupType = "qemu"
				}

				// For shared storage (like PBS), use the storage name as node
				// to avoid confusion about which node the backup is on
				backupNode := node.Node
				isPBSStorage := strings.HasPrefix(storage.Storage, "pbs-") || storage.Type == "pbs"
				if isPBSStorage || storage.Shared == 1 {
					backupNode = storage.Storage // Use storage name for shared storage
				}

				// Check verification status for PBS backups
				verified := false
				verificationInfo := ""
				if isPBSStorage {
					// Check if verified flag is set
					if content.Verified > 0 {
						verified = true
					}
					// Also check verification map if available
					if content.Verification != nil {
						if state, ok := content.Verification["state"].(string); ok {
							verified = (state == "ok")
							verificationInfo = state
						}
					}
				}

				backup := models.StorageBackup{
					ID:           fmt.Sprintf("%s-%s", instanceName, content.Volid),
					Storage:      storage.Storage,
					Node:         backupNode,
					Type:         backupType,
					VMID:         content.VMID,
					Time:         time.Unix(content.CTime, 0),
					CTime:        content.CTime,
					Size:         int64(content.Size),
					Format:       content.Format,
					Notes:        content.Notes,
					Protected:    content.Protected > 0,
					Volid:        content.Volid,
					IsPBS:        isPBSStorage,
					Verified:     verified,
					Verification: verificationInfo,
				}

				allBackups = append(allBackups, backup)
			}
		}
	}

	// Update state with storage backups for this instance
	m.state.UpdateStorageBackupsForInstance(instanceName, allBackups)

	log.Debug().
		Str("instance", instanceName).
		Int("count", len(allBackups)).
		Msg("Storage backups polled")
}

// pollGuestSnapshots polls snapshots for all VMs and containers
func (m *Monitor) pollGuestSnapshots(ctx context.Context, instanceName string, client PVEClientInterface) {
	log.Debug().Str("instance", instanceName).Msg("Polling guest snapshots")

	// Create a separate context with a longer timeout for snapshot queries
	// Snapshot queries can be slow, especially with many VMs/containers
	snapshotCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Get current VMs and containers from state
	m.mu.RLock()
	vms := append([]models.VM{}, m.state.VMs...)
	containers := append([]models.Container{}, m.state.Containers...)
	m.mu.RUnlock()

	var allSnapshots []models.GuestSnapshot

	// Poll VM snapshots
	for _, vm := range vms {
		// Skip templates
		if vm.Template {
			continue
		}

		snapshots, err := client.GetVMSnapshots(snapshotCtx, vm.Node, vm.VMID)
		if err != nil {
			// This is common for VMs without snapshots, so use debug level
			monErr := errors.NewMonitorError(errors.ErrorTypeAPI, "get_vm_snapshots", instanceName, err).WithNode(vm.Node)
			log.Debug().
				Err(monErr).
				Str("node", vm.Node).
				Int("vmid", vm.VMID).
				Msg("Failed to get VM snapshots")
			continue
		}

		for _, snap := range snapshots {
			snapshot := models.GuestSnapshot{
				ID:          fmt.Sprintf("%s-%s-%d-%s", instanceName, vm.Node, vm.VMID, snap.Name),
				Name:        snap.Name,
				Node:        vm.Node,
				Type:        "qemu",
				VMID:        vm.VMID,
				Time:        time.Unix(snap.SnapTime, 0),
				Description: snap.Description,
				Parent:      snap.Parent,
				VMState:     true, // VM state support enabled
			}

			allSnapshots = append(allSnapshots, snapshot)
		}
	}

	// Poll container snapshots
	for _, ct := range containers {
		// Skip templates
		if ct.Template {
			continue
		}

		snapshots, err := client.GetContainerSnapshots(snapshotCtx, ct.Node, ct.VMID)
		if err != nil {
			// API error 596 means snapshots not supported/available - this is expected for many containers
			errStr := err.Error()
			if strings.Contains(errStr, "596") || strings.Contains(errStr, "not available") {
				// Silently skip containers without snapshot support
				continue
			}
			// Log other errors at debug level
			monErr := errors.NewMonitorError(errors.ErrorTypeAPI, "get_container_snapshots", instanceName, err).WithNode(ct.Node)
			log.Debug().
				Err(monErr).
				Str("node", ct.Node).
				Int("vmid", ct.VMID).
				Msg("Failed to get container snapshots")
			continue
		}

		for _, snap := range snapshots {
			snapshot := models.GuestSnapshot{
				ID:          fmt.Sprintf("%s-%s-%d-%s", instanceName, ct.Node, ct.VMID, snap.Name),
				Name:        snap.Name,
				Node:        ct.Node,
				Type:        "lxc",
				VMID:        ct.VMID,
				Time:        time.Unix(snap.SnapTime, 0),
				Description: snap.Description,
				Parent:      snap.Parent,
				VMState:     false,
			}

			allSnapshots = append(allSnapshots, snapshot)
		}
	}

	// Update state with guest snapshots for this instance
	m.state.UpdateGuestSnapshotsForInstance(instanceName, allSnapshots)

	log.Debug().
		Str("instance", instanceName).
		Int("count", len(allSnapshots)).
		Msg("Guest snapshots polled")
}

// Stop gracefully stops the monitor
func (m *Monitor) Stop() {
	log.Info().Msg("Stopping monitor")

	// Stop the alert manager to save history
	if m.alertManager != nil {
		m.alertManager.Stop()
	}

	// Stop notification manager
	if m.notificationMgr != nil {
		m.notificationMgr.Stop()
	}

	log.Info().Msg("Monitor stopped")
}

// recordAuthFailure records an authentication failure for a node
func (m *Monitor) recordAuthFailure(instanceName string, nodeType string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	nodeID := instanceName
	if nodeType != "" {
		nodeID = nodeType + "-" + instanceName
	}

	// Increment failure count
	m.authFailures[nodeID]++
	m.lastAuthAttempt[nodeID] = time.Now()

	log.Warn().
		Str("node", nodeID).
		Int("failures", m.authFailures[nodeID]).
		Msg("Authentication failure recorded")

	// If we've exceeded the threshold, remove the node
	const maxAuthFailures = 5
	if m.authFailures[nodeID] >= maxAuthFailures {
		log.Error().
			Str("node", nodeID).
			Int("failures", m.authFailures[nodeID]).
			Msg("Maximum authentication failures reached, removing node from state")

		// Remove from state based on type
		if nodeType == "pve" {
			m.removeFailedPVENode(instanceName)
		} else if nodeType == "pbs" {
			m.removeFailedPBSNode(instanceName)
		}

		// Reset the counter since we've removed the node
		delete(m.authFailures, nodeID)
		delete(m.lastAuthAttempt, nodeID)
	}
}

// resetAuthFailures resets the failure count for a node after successful auth
func (m *Monitor) resetAuthFailures(instanceName string, nodeType string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	nodeID := instanceName
	if nodeType != "" {
		nodeID = nodeType + "-" + instanceName
	}

	if count, exists := m.authFailures[nodeID]; exists && count > 0 {
		log.Info().
			Str("node", nodeID).
			Int("previousFailures", count).
			Msg("Authentication succeeded, resetting failure count")

		delete(m.authFailures, nodeID)
		delete(m.lastAuthAttempt, nodeID)
	}
}

// removeFailedPVENode updates a PVE node to show failed authentication status
func (m *Monitor) removeFailedPVENode(instanceName string) {
	// Get instance config to get host URL
	var hostURL string
	for _, cfg := range m.config.PVEInstances {
		if cfg.Name == instanceName {
			hostURL = cfg.Host
			break
		}
	}

	// Create a failed node entry to show in UI with error status
	failedNode := models.Node{
		ID:               instanceName + "-failed",
		Name:             instanceName,
		Instance:         instanceName,
		Host:             hostURL, // Include host URL even for failed nodes
		Status:           "offline",
		Type:             "node",
		ConnectionHealth: "error",
		LastSeen:         time.Now(),
		// Set other fields to zero values to indicate no data
		CPU:    0,
		Memory: models.Memory{},
		Disk:   models.Disk{},
	}

	// Update with just the failed node
	m.state.UpdateNodesForInstance(instanceName, []models.Node{failedNode})

	// Remove all other resources associated with this instance
	m.state.UpdateVMsForInstance(instanceName, []models.VM{})
	m.state.UpdateContainersForInstance(instanceName, []models.Container{})
	m.state.UpdateStorageForInstance(instanceName, []models.Storage{})
	m.state.UpdateBackupTasksForInstance(instanceName, []models.BackupTask{})
	m.state.UpdateStorageBackupsForInstance(instanceName, []models.StorageBackup{})
	m.state.UpdateGuestSnapshotsForInstance(instanceName, []models.GuestSnapshot{})

	// Set connection health to false
	m.state.SetConnectionHealth(instanceName, false)
}

// removeFailedPBSNode removes a PBS node and all its resources from state
func (m *Monitor) removeFailedPBSNode(instanceName string) {
	// Remove PBS instance by passing empty array
	currentInstances := m.state.PBSInstances
	var updatedInstances []models.PBSInstance
	for _, inst := range currentInstances {
		if inst.Name != instanceName {
			updatedInstances = append(updatedInstances, inst)
		}
	}
	m.state.UpdatePBSInstances(updatedInstances)

	// Remove PBS backups
	m.state.UpdatePBSBackups(instanceName, []models.PBSBackup{})

	// Set connection health to false
	m.state.SetConnectionHealth("pbs-"+instanceName, false)
}

// pollPBSBackups fetches all backups from PBS datastores
func (m *Monitor) pollPBSBackups(ctx context.Context, instanceName string, client *pbs.Client, datastores []models.PBSDatastore) {
	log.Debug().Str("instance", instanceName).Msg("Polling PBS backups")

	var allBackups []models.PBSBackup

	// Process each datastore
	for _, ds := range datastores {
		// Get namespace paths
		namespacePaths := make([]string, 0, len(ds.Namespaces))
		for _, ns := range ds.Namespaces {
			namespacePaths = append(namespacePaths, ns.Path)
		}

		log.Info().
			Str("instance", instanceName).
			Str("datastore", ds.Name).
			Int("namespaces", len(namespacePaths)).
			Strs("namespace_paths", namespacePaths).
			Msg("Processing datastore namespaces")

		// Fetch backups from all namespaces concurrently
		backupsMap, err := client.ListAllBackups(ctx, ds.Name, namespacePaths)
		if err != nil {
			log.Error().Err(err).
				Str("instance", instanceName).
				Str("datastore", ds.Name).
				Msg("Failed to fetch PBS backups")
			continue
		}

		// Convert PBS backups to model backups
		for namespace, snapshots := range backupsMap {
			for _, snapshot := range snapshots {
				backupTime := time.Unix(snapshot.BackupTime, 0)

				// Generate unique ID
				id := fmt.Sprintf("pbs-%s-%s-%s-%s-%s-%d",
					instanceName, ds.Name, namespace,
					snapshot.BackupType, snapshot.BackupID,
					snapshot.BackupTime)

				// Extract file names from files (which can be strings or objects)
				var fileNames []string
				for _, file := range snapshot.Files {
					switch f := file.(type) {
					case string:
						fileNames = append(fileNames, f)
					case map[string]interface{}:
						if filename, ok := f["filename"].(string); ok {
							fileNames = append(fileNames, filename)
						}
					}
				}

				// Extract verification status
				verified := false
				if snapshot.Verification != nil {
					switch v := snapshot.Verification.(type) {
					case string:
						verified = v == "ok"
					case map[string]interface{}:
						if state, ok := v["state"].(string); ok {
							verified = state == "ok"
						}
					}

					// Debug log verification data
					log.Debug().
						Str("vmid", snapshot.BackupID).
						Int64("time", snapshot.BackupTime).
						Interface("verification", snapshot.Verification).
						Bool("verified", verified).
						Msg("PBS backup verification status")
				}

				backup := models.PBSBackup{
					ID:         id,
					Instance:   instanceName,
					Datastore:  ds.Name,
					Namespace:  namespace,
					BackupType: snapshot.BackupType,
					VMID:       snapshot.BackupID,
					BackupTime: backupTime,
					Size:       snapshot.Size,
					Protected:  snapshot.Protected,
					Verified:   verified,
					Comment:    snapshot.Comment,
					Files:      fileNames,
				}

				allBackups = append(allBackups, backup)
			}
		}
	}

	log.Info().
		Str("instance", instanceName).
		Int("count", len(allBackups)).
		Msg("PBS backups fetched")

	// Update state
	m.state.UpdatePBSBackups(instanceName, allBackups)
}
