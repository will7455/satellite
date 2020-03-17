package node

import (
	"time"

	pb "github.com/gravitational/satellite/agent/proto/agentpb"
)

// filterByTimestamp filters out events that occurred before the provided
// timestamp.
func filterByTimestamp(events []*pb.TimelineEvent, timestamp time.Time) (filtered []*pb.TimelineEvent) {
	for _, event := range events {
		if event.GetTimestamp().ToTime().Before(timestamp) {
			continue
		}
		filtered = append(filtered, event)
	}
	return filtered
}

// hasRoleMaster returns true if tags contains role 'master'.
func hasRoleMaster(tags map[string]string) bool {
	role, ok := tags["role"]
	return ok && Role(role) == RoleMaster
}
