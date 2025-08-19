package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rcourtman/pulse-go-rewrite/pkg/tlsutil"
	"github.com/rs/zerolog/log"
)

// FlexInt handles JSON fields that can be int, float, or string (for cpulimit support)
type FlexInt int

func (f *FlexInt) UnmarshalJSON(data []byte) error {
	// Try to unmarshal as int first
	var i int
	if err := json.Unmarshal(data, &i); err == nil {
		*f = FlexInt(i)
		return nil
	}

	// Try as float (handles cpulimit like 1.5)
	var fl float64
	if err := json.Unmarshal(data, &fl); err == nil {
		*f = FlexInt(int(fl))
		return nil
	}

	// If that fails, try as string
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}

	// Parse string to float first (handles "1.5" format from cpulimit)
	floatVal, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}

	// Convert to int
	*f = FlexInt(int(floatVal))
	return nil
}

// Client represents a Proxmox VE API client
type Client struct {
	baseURL    string
	httpClient *http.Client
	auth       auth
	config     ClientConfig
}

// ClientConfig holds configuration for the Proxmox client
type ClientConfig struct {
	Host        string
	User        string
	Password    string
	TokenName   string
	TokenValue  string
	Fingerprint string
	VerifySSL   bool
	Timeout     time.Duration
}

// auth represents authentication details
type auth struct {
	user       string
	realm      string
	ticket     string
	csrfToken  string
	tokenName  string
	tokenValue string
	expiresAt  time.Time
}

// NewClient creates a new Proxmox VE API client
func NewClient(cfg ClientConfig) (*Client, error) {
	var user, realm string

	// Log what auth method we're using
	log.Debug().
		Str("host", cfg.Host).
		Bool("hasToken", cfg.TokenName != "").
		Bool("hasPassword", cfg.Password != "").
		Str("tokenName", cfg.TokenName).
		Str("user", cfg.User).
		Msg("Creating Proxmox client")

	// For token authentication, we don't need user@realm format
	if cfg.TokenName != "" && cfg.TokenValue != "" {
		// Extract user and realm from token name (format: user@realm!tokenname)
		if strings.Contains(cfg.TokenName, "@") && strings.Contains(cfg.TokenName, "!") {
			parts := strings.Split(cfg.TokenName, "!")
			if len(parts) == 2 {
				userRealm := parts[0]
				userRealmParts := strings.Split(userRealm, "@")
				if len(userRealmParts) == 2 {
					user = userRealmParts[0]
					realm = userRealmParts[1]
				}
			}
		}
	} else {
		// For password authentication, parse user and realm from User field
		parts := strings.Split(cfg.User, "@")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid user format, expected user@realm")
		}
		user = parts[0]
		realm = parts[1]
	}

	// Create HTTP client with proper TLS configuration
	httpClient := tlsutil.CreateHTTPClient(cfg.VerifySSL, cfg.Fingerprint)
	// Override timeout if specified
	if cfg.Timeout > 0 {
		httpClient.Timeout = cfg.Timeout
	}

	// Extract just the token name part for API token authentication
	tokenName := cfg.TokenName
	if cfg.TokenName != "" && strings.Contains(cfg.TokenName, "!") {
		parts := strings.Split(cfg.TokenName, "!")
		if len(parts) == 2 {
			tokenName = parts[1] // Just the token name part (e.g., "pulse-token")
		}
	}

	log.Debug().
		Str("user", user).
		Str("realm", realm).
		Bool("hasToken", cfg.TokenValue != "").
		Msg("Proxmox client configured")

	client := &Client{
		baseURL:    strings.TrimSuffix(cfg.Host, "/") + "/api2/json",
		httpClient: httpClient,
		config:     cfg,
		auth: auth{
			user:       user,
			realm:      realm,
			tokenName:  tokenName,
			tokenValue: cfg.TokenValue,
		},
	}

	// Authenticate if using password
	if cfg.Password != "" && cfg.TokenName == "" {
		if err := client.authenticate(context.Background()); err != nil {
			return nil, fmt.Errorf("authentication failed: %w", err)
		}
	}

	return client, nil
}

// authenticate performs password-based authentication
func (c *Client) authenticate(ctx context.Context) error {
	data := url.Values{
		"username": {c.auth.user + "@" + c.auth.realm},
		"password": {c.config.Password},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/access/ticket", strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("authentication failed (status %d): %s", resp.StatusCode, string(body))
		}
		return fmt.Errorf("authentication failed: %s", string(body))
	}

	var result struct {
		Data struct {
			Ticket              string `json:"ticket"`
			CSRFPreventionToken string `json:"CSRFPreventionToken"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	c.auth.ticket = result.Data.Ticket
	c.auth.csrfToken = result.Data.CSRFPreventionToken
	c.auth.expiresAt = time.Now().Add(2 * time.Hour) // PVE tickets expire after 2 hours

	return nil
}

// request performs an API request
func (c *Client) request(ctx context.Context, method, path string, data url.Values) (*http.Response, error) {
	// Re-authenticate if needed
	if c.config.Password != "" && c.auth.tokenName == "" && time.Now().After(c.auth.expiresAt) {
		if err := c.authenticate(ctx); err != nil {
			return nil, fmt.Errorf("re-authentication failed: %w", err)
		}
	}

	var body io.Reader
	if data != nil {
		body = strings.NewReader(data.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}

	// Set headers
	if data != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	// Set authentication
	if c.auth.tokenName != "" && c.auth.tokenValue != "" {
		// API token authentication
		authHeader := fmt.Sprintf("PVEAPIToken=%s@%s!%s=%s",
			c.auth.user, c.auth.realm, c.auth.tokenName, c.auth.tokenValue)
		req.Header.Set("Authorization", authHeader)
		// NEVER log the actual token value - only log that we're using token auth
		maskedHeader := fmt.Sprintf("PVEAPIToken=%s@%s!%s=***",
			c.auth.user, c.auth.realm, c.auth.tokenName)
		log.Debug().
			Str("authHeader", maskedHeader).
			Str("url", req.URL.String()).
			Msg("Setting API token authentication")
	} else if c.auth.ticket != "" {
		// Ticket authentication
		req.Header.Set("Cookie", "PVEAuthCookie="+c.auth.ticket)
		if method != "GET" && c.auth.csrfToken != "" {
			req.Header.Set("CSRFPreventionToken", c.auth.csrfToken)
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	// Check for errors
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		// Create base error
		err := fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))

		// Log auth issues for debugging (595 is Proxmox "no ticket" error)
		if resp.StatusCode == 595 || resp.StatusCode == 401 || resp.StatusCode == 403 {
			log.Warn().
				Str("url", req.URL.String()).
				Int("status", resp.StatusCode).
				Bool("hasToken", c.config.TokenName != "").
				Bool("hasPassword", c.config.Password != "").
				Str("tokenName", c.config.TokenName).
				Msg("Proxmox authentication error")
		}

		// Wrap with appropriate error type
		if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 595 {
			// Import errors package at top of file
			return nil, fmt.Errorf("authentication error: %w", err)
		}

		return nil, err
	}

	return resp, nil
}

// get performs a GET request
func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	return c.request(ctx, "GET", path, nil)
}

// post performs a POST request
func (c *Client) post(ctx context.Context, path string, data url.Values) (*http.Response, error) {
	return c.request(ctx, "POST", path, data)
}

// Node represents a Proxmox VE node
type Node struct {
	Node    string  `json:"node"`
	Status  string  `json:"status"`
	CPU     float64 `json:"cpu"`
	MaxCPU  int     `json:"maxcpu"`
	Mem     uint64  `json:"mem"`
	MaxMem  uint64  `json:"maxmem"`
	Disk    uint64  `json:"disk"`
	MaxDisk uint64  `json:"maxdisk"`
	Uptime  uint64  `json:"uptime"`
	Level   string  `json:"level"`
}

// NodeStatus represents detailed node status from /nodes/{node}/status endpoint
// This endpoint provides real-time metrics that update every second
type NodeStatus struct {
	CPU           float64       `json:"cpu"`     // Real-time CPU usage (0-1)
	Memory        *MemoryStatus `json:"memory"`  // Real-time memory stats
	Swap          *SwapStatus   `json:"swap"`    // Swap usage
	LoadAvg       []interface{} `json:"loadavg"` // Can be float64 or string
	KernelVersion string        `json:"kversion"`
	PVEVersion    string        `json:"pveversion"`
	CPUInfo       *CPUInfo      `json:"cpuinfo"`
	RootFS        *RootFS       `json:"rootfs"`
	Uptime        uint64        `json:"uptime"`  // Uptime in seconds
	Wait          float64       `json:"wait"`    // IO wait
	IODelay       float64       `json:"iodelay"` // IO delay
	Idle          float64       `json:"idle"`    // CPU idle time
}

// MemoryStatus represents real-time memory information
type MemoryStatus struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Free  uint64 `json:"free"`
}

// SwapStatus represents swap information
type SwapStatus struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Free  uint64 `json:"free"`
}

// RootFS represents root filesystem information
type RootFS struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Free  uint64 `json:"avail"`
}

// CPUInfo represents CPU information
type CPUInfo struct {
	Model   string      `json:"model"`
	Cores   int         `json:"cores"`
	Sockets int         `json:"sockets"`
	MHz     interface{} `json:"mhz"` // Can be string or number
}

// GetMHzString returns MHz as a string
func (c *CPUInfo) GetMHzString() string {
	if c.MHz == nil {
		return ""
	}
	switch v := c.MHz.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	case int:
		return fmt.Sprintf("%d", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// GetNodes returns all nodes in the cluster
func (c *Client) GetNodes(ctx context.Context) ([]Node, error) {
	resp, err := c.get(ctx, "/nodes")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []Node `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Data, nil
}

// GetNodeStatus returns detailed status for a specific node
func (c *Client) GetNodeStatus(ctx context.Context, node string) (*NodeStatus, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/nodes/%s/status", node))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data NodeStatus `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result.Data, nil
}

// VM represents a Proxmox VE virtual machine
type VM struct {
	VMID      int     `json:"vmid"`
	Name      string  `json:"name"`
	Node      string  `json:"node"`
	Status    string  `json:"status"`
	CPU       float64 `json:"cpu"`
	CPUs      int     `json:"cpus"`
	Mem       uint64  `json:"mem"`
	MaxMem    uint64  `json:"maxmem"`
	Disk      uint64  `json:"disk"`
	MaxDisk   uint64  `json:"maxdisk"`
	NetIn     uint64  `json:"netin"`
	NetOut    uint64  `json:"netout"`
	DiskRead  uint64  `json:"diskread"`
	DiskWrite uint64  `json:"diskwrite"`
	Uptime    uint64  `json:"uptime"`
	Template  int     `json:"template"`
	Tags      string  `json:"tags"`
	Lock      string  `json:"lock"`
}

// Container represents a Proxmox VE LXC container
type Container struct {
	VMID      int     `json:"vmid"`
	Name      string  `json:"name"`
	Node      string  `json:"node"`
	Status    string  `json:"status"`
	CPU       float64 `json:"cpu"`
	CPUs      FlexInt `json:"cpus"`
	Mem       uint64  `json:"mem"`
	MaxMem    uint64  `json:"maxmem"`
	Swap      uint64  `json:"swap"`
	MaxSwap   uint64  `json:"maxswap"`
	Disk      uint64  `json:"disk"`
	MaxDisk   uint64  `json:"maxdisk"`
	NetIn     uint64  `json:"netin"`
	NetOut    uint64  `json:"netout"`
	DiskRead  uint64  `json:"diskread"`
	DiskWrite uint64  `json:"diskwrite"`
	Uptime    uint64  `json:"uptime"`
	Template  int     `json:"template"`
	Tags      string  `json:"tags"`
	Lock      string  `json:"lock"`
}

// Storage represents a Proxmox VE storage
type Storage struct {
	Storage   string `json:"storage"`
	Type      string `json:"type"`
	Content   string `json:"content"`
	Active    int    `json:"active"`
	Enabled   int    `json:"enabled"`
	Shared    int    `json:"shared"`
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"`
	Available uint64 `json:"avail"`
}

// StorageContent represents content in a storage
type StorageContent struct {
	Volid        string                 `json:"volid"`
	Content      string                 `json:"content"`
	CTime        int64                  `json:"ctime"`
	Format       string                 `json:"format"`
	Size         uint64                 `json:"size"`
	VMID         int                    `json:"vmid"`
	Notes        string                 `json:"notes"`
	Protected    int                    `json:"protected"`
	Encryption   string                 `json:"encryption"`
	Verification map[string]interface{} `json:"verification"` // PBS verification info
	Verified     int                    `json:"verified"`     // Simple verified flag
}

// Snapshot represents a VM or container snapshot
type Snapshot struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	SnapTime    int64  `json:"snaptime"`
	Parent      string `json:"parent"`
	VMID        int    `json:"vmid"`
}

// GetVMs returns all VMs on a specific node
func (c *Client) GetVMs(ctx context.Context, node string) ([]VM, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/nodes/%s/qemu", node))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []VM `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Data, nil
}

// GetContainers returns all LXC containers on a specific node
func (c *Client) GetContainers(ctx context.Context, node string) ([]Container, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/nodes/%s/lxc", node))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []Container `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Data, nil
}

// GetStorage returns storage information for a specific node
func (c *Client) GetStorage(ctx context.Context, node string) ([]Storage, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/nodes/%s/storage", node))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []Storage `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Data, nil
}

// GetAllStorage returns storage information across all nodes
func (c *Client) GetAllStorage(ctx context.Context) ([]Storage, error) {
	resp, err := c.get(ctx, "/storage")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []Storage `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Data, nil
}

// Task represents a Proxmox task
type Task struct {
	UPID      string `json:"upid"`
	Node      string `json:"node"`
	PID       int    `json:"pid"`
	PStart    int64  `json:"pstart"`
	StartTime int64  `json:"starttime"`
	Type      string `json:"type"`
	ID        string `json:"id"`
	User      string `json:"user"`
	Status    string `json:"status,omitempty"`
	EndTime   int64  `json:"endtime,omitempty"`
}

// GetNodeTasks gets tasks for a specific node
func (c *Client) GetNodeTasks(ctx context.Context, node string) ([]Task, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/nodes/%s/tasks", node))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []Task `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Data, nil
}

// GetBackupTasks gets all backup tasks across all nodes
func (c *Client) GetBackupTasks(ctx context.Context) ([]Task, error) {
	// First get all nodes
	nodes, err := c.GetNodes(ctx)
	if err != nil {
		return nil, err
	}

	var allTasks []Task
	for _, node := range nodes {
		if node.Status != "online" {
			continue
		}

		tasks, err := c.GetNodeTasks(ctx, node.Node)
		if err != nil {
			// Log error but continue with other nodes
			continue
		}

		// Filter for backup tasks
		for _, task := range tasks {
			if task.Type == "vzdump" {
				allTasks = append(allTasks, task)
			}
		}
	}

	return allTasks, nil
}

// GetStorageContent returns the content of a specific storage
func (c *Client) GetStorageContent(ctx context.Context, node, storage string) ([]StorageContent, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content", node, storage))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []StorageContent `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	// Filter for backup content only
	var backups []StorageContent
	for _, content := range result.Data {
		if content.Content == "backup" || content.Content == "vztmpl" {
			backups = append(backups, content)
		}
	}

	return backups, nil
}

// GetVMSnapshots returns snapshots for a specific VM
func (c *Client) GetVMSnapshots(ctx context.Context, node string, vmid int) ([]Snapshot, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/snapshot", node, vmid))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []Snapshot `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	// Filter out the 'current' snapshot which is not a real snapshot
	var snapshots []Snapshot
	for _, snap := range result.Data {
		if snap.Name != "current" {
			snap.VMID = vmid
			snapshots = append(snapshots, snap)
		}
	}

	return snapshots, nil
}

// GetContainerSnapshots returns snapshots for a specific container
func (c *Client) GetContainerSnapshots(ctx context.Context, node string, vmid int) ([]Snapshot, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/nodes/%s/lxc/%d/snapshot", node, vmid))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []Snapshot `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	// Filter out the 'current' snapshot which is not a real snapshot
	var snapshots []Snapshot
	for _, snap := range result.Data {
		if snap.Name != "current" {
			snap.VMID = vmid
			snapshots = append(snapshots, snap)
		}
	}

	return snapshots, nil
}

// ClusterStatus represents the cluster status response
type ClusterStatus struct {
	Type    string `json:"type"`    // "cluster" or "node"
	ID      string `json:"id"`      // Node ID or cluster name
	Name    string `json:"name"`    // Node name
	IP      string `json:"ip"`      // Node IP address
	Local   int    `json:"local"`   // 1 if this is the local node
	Nodeid  int    `json:"nodeid"`  // Node ID in cluster
	Online  int    `json:"online"`  // 1 if online
	Level   string `json:"level"`   // Connection level
	Quorate int    `json:"quorate"` // 1 if cluster has quorum
}

// GetClusterStatus returns the cluster status including all nodes
func (c *Client) GetClusterStatus(ctx context.Context) ([]ClusterStatus, error) {
	resp, err := c.get(ctx, "/cluster/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []ClusterStatus `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Data, nil
}

// IsClusterMember checks if this node is part of a cluster
func (c *Client) IsClusterMember(ctx context.Context) (bool, error) {
	status, err := c.GetClusterStatus(ctx)
	if err != nil {
		// If we can't get cluster status, assume it's not a cluster
		// This prevents treating API errors as cluster membership
		return false, nil
	}

	// Check for explicit cluster entry (most reliable indicator)
	for _, s := range status {
		if s.Type == "cluster" {
			// Found a cluster entry - this is definitely a cluster
			return true, nil
		}
	}

	// Fallback: If we have more than one node entry, it's likely a cluster
	// (though this shouldn't happen without a cluster entry)
	nodeCount := 0
	for _, s := range status {
		if s.Type == "node" {
			nodeCount++
		}
	}

	return nodeCount > 1, nil
}

// GetClusterNodes returns all nodes in the cluster with their connection info
func (c *Client) GetClusterNodes(ctx context.Context) ([]ClusterStatus, error) {
	status, err := c.GetClusterStatus(ctx)
	if err != nil {
		return nil, err
	}

	var nodes []ClusterStatus
	for _, s := range status {
		if s.Type == "node" {
			nodes = append(nodes, s)
		}
	}

	return nodes, nil
}

// GetVMConfig returns the configuration for a specific VM
func (c *Client) GetVMConfig(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data map[string]interface{} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Data, nil
}

// GetVMAgentInfo returns guest agent information for a VM if available
func (c *Client) GetVMAgentInfo(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/agent/get-osinfo", node, vmid))
	if err != nil {
		// Guest agent might not be installed or running
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data map[string]interface{} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Data, nil
}

// GetVMStatus returns detailed VM status including balloon info
func (c *Client) GetVMStatus(ctx context.Context, node string, vmid int) (*VMStatus, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/current", node, vmid))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data VMStatus `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result.Data, nil
}

// GetContainerStatus returns detailed container status using real-time endpoint
func (c *Client) GetContainerStatus(ctx context.Context, node string, vmid int) (*Container, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/nodes/%s/lxc/%d/status/current", node, vmid))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data Container `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result.Data, nil
}

// ClusterResource represents a resource from /cluster/resources
type ClusterResource struct {
	ID        string  `json:"id"`
	Type      string  `json:"type"`
	Node      string  `json:"node"`
	Status    string  `json:"status"`
	Name      string  `json:"name,omitempty"`
	VMID      int     `json:"vmid,omitempty"`
	CPU       float64 `json:"cpu,omitempty"`
	MaxCPU    int     `json:"maxcpu,omitempty"`
	Mem       uint64  `json:"mem,omitempty"`
	MaxMem    uint64  `json:"maxmem,omitempty"`
	Disk      uint64  `json:"disk,omitempty"`
	MaxDisk   uint64  `json:"maxdisk,omitempty"`
	NetIn     uint64  `json:"netin,omitempty"`
	NetOut    uint64  `json:"netout,omitempty"`
	DiskRead  uint64  `json:"diskread,omitempty"`
	DiskWrite uint64  `json:"diskwrite,omitempty"`
	Uptime    uint64  `json:"uptime,omitempty"`
	Template  int     `json:"template,omitempty"`
	Tags      string  `json:"tags,omitempty"`
}

// GetClusterResources returns all resources (VMs, containers) across the cluster
func (c *Client) GetClusterResources(ctx context.Context, resourceType string) ([]ClusterResource, error) {
	path := "/cluster/resources"
	if resourceType != "" {
		path = fmt.Sprintf("%s?type=%s", path, resourceType)
	}

	resp, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []ClusterResource `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Data, nil
}

// VMStatus represents detailed VM status
type VMStatus struct {
	Status     string  `json:"status"`
	CPU        float64 `json:"cpu"`
	CPUs       int     `json:"cpus"`
	Mem        uint64  `json:"mem"`
	MaxMem     uint64  `json:"maxmem"`
	Balloon    uint64  `json:"balloon"`
	BalloonMin uint64  `json:"balloon_min"`
	FreeMem    uint64  `json:"freemem"`
	MemInfo    *struct {
		Used  uint64 `json:"used"`
		Free  uint64 `json:"free"`
		Total uint64 `json:"total"`
	} `json:"meminfo,omitempty"`
	Disk      uint64 `json:"disk"`
	MaxDisk   uint64 `json:"maxdisk"`
	DiskRead  uint64 `json:"diskread"`
	DiskWrite uint64 `json:"diskwrite"`
	NetIn     uint64 `json:"netin"`
	NetOut    uint64 `json:"netout"`
	Uptime    uint64 `json:"uptime"`
	Agent     int    `json:"agent"`
}
