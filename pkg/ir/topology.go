package ir

import "strings"

// TopologyTier describes where an arena's backing memory lives.
type TopologyTier int

const (
	// DRAMLocal is node-local DRAM.
	DRAMLocal TopologyTier = iota
	// CXLPooled is a pooled CXL memory tier, reachable over the fabric.
	CXLPooled
	// Persistent is byte-addressable persistent memory.
	Persistent
)

var topologyNames = map[TopologyTier]string{
	DRAMLocal:  "DRAM_LOCAL",
	CXLPooled:  "CXL_POOLED",
	Persistent: "PERSISTENT",
}

// String returns the textual spelling of the topology tier.
func (t TopologyTier) String() string {
	if n, ok := topologyNames[t]; ok {
		return n
	}
	return "UNKNOWN"
}

// ParseTopology resolves a textual topology name (case-insensitive).
func ParseTopology(s string) (TopologyTier, bool) {
	up := strings.ToUpper(strings.TrimSpace(s))
	for t, n := range topologyNames {
		if n == up {
			return t, true
		}
	}
	return DRAMLocal, false
}
