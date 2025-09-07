package types

import (
	"time"
)

// Config holds registry configuration.
type Config struct {
	ClusterName string
	Region      string
	// Optional filters
	Services       []string // only these ECS services (names) if non-empty
	TaskFamily     []string // restrict to task definition families
	PollInterval   time.Duration
	EnableENIQuery bool   // query EC2 to resolve ENI->IP (true recommended)
	TrustDomain    string // for SPIFFE mapping
}
