package ecs

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ecst "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// collectENIIDs scans a slice of ECS tasks and returns all referenced ENI IDs.
// It looks in task Attachments for Attachment.Type == "ElasticNetworkInterface"
// and reads the "networkInterfaceId" detail.
func CollectENIIDs(tasks []ecst.Task) []string {
	ids := make([]string, 0)
	for _, t := range tasks {
		for _, att := range t.Attachments {
			// Attachment.Type may be nil in some SDK versions
			if att.Type == nil {
				continue
			}
			if strings.EqualFold(*att.Type, "ElasticNetworkInterface") {
				for _, d := range att.Details {
					if d.Name != nil && *d.Name == "networkInterfaceId" && d.Value != nil {
						ids = append(ids, *d.Value)
					}
				}
			}
		}
	}
	return ids
}

// describeENIs calls EC2 DescribeNetworkInterfaces for the given ENI IDs
// and returns a map from ENI ID -> private IPv4 address. It attempts to
// handle the common shapes of the response (PrivateIpAddress, PrivateIpAddresses[]).
func DescribeENIs(ctx context.Context, cli *ec2.Client, eniIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(eniIDs) == 0 {
		return out, nil
	}

	// DescribeNetworkInterfaces supports up to 100 ids per call; if the slice
	// is larger we should batch. For simplicity, do batching here.
	const batchSize = 100
	for i := 0; i < len(eniIDs); i += batchSize {
		j := i + batchSize
		if j > len(eniIDs) {
			j = len(eniIDs)
		}
		batch := eniIDs[i:j]

		resp, err := cli.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
			NetworkInterfaceIds: batch,
		})
		if err != nil {
			return nil, fmt.Errorf("DescribeNetworkInterfaces: %w", err)
		}
		for _, ni := range resp.NetworkInterfaces {
			if ni.NetworkInterfaceId == nil {
				continue
			}
			eniID := *ni.NetworkInterfaceId
			// Prefer direct PrivateIpAddress
			if ni.PrivateIpAddress != nil && *ni.PrivateIpAddress != "" {
				out[eniID] = *ni.PrivateIpAddress
				continue
			}
			// Fallback: the PrivateIpAddresses list
			if len(ni.PrivateIpAddresses) > 0 {
				for _, pip := range ni.PrivateIpAddresses {
					if pip.PrivateIpAddress != nil && *pip.PrivateIpAddress != "" {
						out[eniID] = *pip.PrivateIpAddress
						break
					}
				}
			}
			// If still empty, try to look at association/public/private ip fields - handled above sufficiently for our use
		}
	}

	return out, nil
}

// getPrivateIPFromAttachmentDetails examines an ECS attachment's Details and
// returns any privateIPv4Address value if present (some ECS responses may carry it here).
func getPrivateIPFromAttachmentDetails(details []ecst.KeyValuePair) string {
	for _, d := range details {
		if d.Name != nil && *d.Name == "privateIPv4Address" && d.Value != nil {
			return *d.Value
		}
	}
	return ""
}

// taskENIIDFromAttachments attempts to extract a networkInterfaceId from attachment details.
// Returns empty if not found.
func taskENIIDFromAttachments(details []ecst.KeyValuePair) string {
	for _, d := range details {
		if d.Name != nil && *d.Name == "networkInterfaceId" && d.Value != nil {
			return *d.Value
		}
	}
	return ""
}

// extractTaskPrivateIP tries a few locations in the task structure to find a private IP.
// This utility is complementary to the logic in conversion.go.taskPrivateIP; you can pick one approach.
// It returns (ip, found).
func extractTaskPrivateIP(t ecst.Task, eniToIP map[string]string) (string, bool) {
	// 1) attachment details (privateIPv4Address)
	for _, att := range t.Attachments {
		if ip := getPrivateIPFromAttachmentDetails(att.Details); ip != "" {
			return ip, true
		}
		// 2) networkInterfaceId -> map lookup via eniToIP
		if eni := taskENIIDFromAttachments(att.Details); eni != "" {
			if ip, ok := eniToIP[eni]; ok && ip != "" {
				return ip, true
			}
		}
	}

	// 3) Container-level networkInterfaces (some SDK shapes put it there)
	for _, c := range t.Containers {
		// Containers may have NetworkInterfaces field with private IPv4 addresses
		for _, nif := range c.NetworkInterfaces {
			if nif.PrivateIpv4Address != nil && *nif.PrivateIpv4Address != "" {
				return *nif.PrivateIpv4Address, true
			}
			// Removed usage of NetworkInterfaceId as it does not exist in the SDK type
		}
	}

	// nothing found
	return "", false
}
