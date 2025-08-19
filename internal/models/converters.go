package models

import (
	"strings"
)

// ToFrontend converts a State to StateFrontend
func (s *State) ToFrontend() StateFrontend {
	// Convert nodes
	nodes := make([]NodeFrontend, len(s.Nodes))
	for i, n := range s.Nodes {
		nodes[i] = n.ToFrontend()
	}

	// Convert VMs
	vms := make([]VMFrontend, len(s.VMs))
	for i, v := range s.VMs {
		vms[i] = v.ToFrontend()
	}

	// Convert containers
	containers := make([]ContainerFrontend, len(s.Containers))
	for i, c := range s.Containers {
		containers[i] = c.ToFrontend()
	}

	// Convert storage
	storage := make([]StorageFrontend, len(s.Storage))
	for i, st := range s.Storage {
		storage[i] = st.ToFrontend()
	}

	return StateFrontend{
		Nodes:            nodes,
		VMs:              vms,
		Containers:       containers,
		Storage:          storage,
		PBS:              s.PBSInstances,
		Metrics:          make(map[string]any),
		PVEBackups:       s.PVEBackups,
		Performance:      make(map[string]any),
		ConnectionHealth: s.ConnectionHealth,
		Stats:            make(map[string]any),
		LastUpdate:       s.LastUpdate.Unix() * 1000, // JavaScript timestamp
	}
}

// ToFrontend converts a Node to NodeFrontend
func (n Node) ToFrontend() NodeFrontend {
	return NodeFrontend{
		ID:               n.ID,
		Node:             n.Name,
		Name:             n.Name,
		Instance:         n.Instance,
		Status:           n.Status,
		Type:             n.Type,
		CPU:              n.CPU,
		Mem:              n.Memory.Used,
		MaxMem:           n.Memory.Total,
		Disk:             n.Disk.Used,
		MaxDisk:          n.Disk.Total,
		Uptime:           n.Uptime,
		LoadAverage:      n.LoadAverage,
		KernelVersion:    n.KernelVersion,
		PVEVersion:       n.PVEVersion,
		CPUInfo:          n.CPUInfo,
		LastSeen:         n.LastSeen.Unix() * 1000,
		ConnectionHealth: n.ConnectionHealth,
	}
}

// ToFrontend converts a VM to VMFrontend
func (v VM) ToFrontend() VMFrontend {
	vm := VMFrontend{
		ID:        v.ID,
		VMID:      v.VMID,
		Name:      v.Name,
		Node:      v.Node,
		Instance:  v.Instance,
		Status:    v.Status,
		Type:      v.Type,
		CPU:       v.CPU,
		CPUs:      v.CPUs,
		Mem:       v.Memory.Used,
		MaxMem:    v.Memory.Total,
		Disk:      v.Disk.Used,
		MaxDisk:   v.Disk.Total,
		NetIn:     zeroIfNegative(v.NetworkIn),
		NetOut:    zeroIfNegative(v.NetworkOut),
		DiskRead:  zeroIfNegative(v.DiskRead),
		DiskWrite: zeroIfNegative(v.DiskWrite),
		Uptime:    v.Uptime,
		Template:  v.Template,
		Lock:      v.Lock,
		LastSeen:  v.LastSeen.Unix() * 1000,
	}

	// Convert tags array to string
	if len(v.Tags) > 0 {
		vm.Tags = strings.Join(v.Tags, ",")
	}

	// Convert last backup time if not zero
	if !v.LastBackup.IsZero() {
		vm.LastBackup = v.LastBackup.Unix() * 1000
	}

	return vm
}

// ToFrontend converts a Container to ContainerFrontend
func (c Container) ToFrontend() ContainerFrontend {
	ct := ContainerFrontend{
		ID:        c.ID,
		VMID:      c.VMID,
		Name:      c.Name,
		Node:      c.Node,
		Instance:  c.Instance,
		Status:    c.Status,
		Type:      c.Type,
		CPU:       c.CPU,
		CPUs:      c.CPUs,
		Mem:       c.Memory.Used,
		MaxMem:    c.Memory.Total,
		Disk:      c.Disk.Used,
		MaxDisk:   c.Disk.Total,
		NetIn:     zeroIfNegative(c.NetworkIn),
		NetOut:    zeroIfNegative(c.NetworkOut),
		DiskRead:  zeroIfNegative(c.DiskRead),
		DiskWrite: zeroIfNegative(c.DiskWrite),
		Uptime:    c.Uptime,
		Template:  c.Template,
		Lock:      c.Lock,
		LastSeen:  c.LastSeen.Unix() * 1000,
	}

	// Convert tags array to string
	if len(c.Tags) > 0 {
		ct.Tags = strings.Join(c.Tags, ",")
	}

	// Convert last backup time if not zero
	if !c.LastBackup.IsZero() {
		ct.LastBackup = c.LastBackup.Unix() * 1000
	}

	return ct
}

// ToFrontend converts Storage to StorageFrontend
func (s Storage) ToFrontend() StorageFrontend {
	return StorageFrontend{
		ID:       s.ID,
		Storage:  s.Name,
		Name:     s.Name,
		Node:     s.Node,
		Instance: s.Instance,
		Type:     s.Type,
		Status:   s.Status,
		Total:    s.Total,
		Used:     s.Used,
		Avail:    s.Free,
		Free:     s.Free,
		Usage:    s.Usage,
		Content:  s.Content,
		Shared:   s.Shared,
		Enabled:  s.Enabled,
		Active:   s.Active,
	}
}

// zeroIfNegative returns 0 for negative values (used for I/O metrics)
func zeroIfNegative(val int64) int64 {
	if val < 0 {
		return 0
	}
	return val
}
