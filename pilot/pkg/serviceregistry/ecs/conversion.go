package ecs

import (
	"fmt"
	"strings"

	ecst "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/ecs/types"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/labels"
)

// BuildModelFromTasks takes ECS tasks + eniToIP map and converts them
// into Istio Services and ServiceInstances.
func BuildModelFromTasks(tasks []ecst.Task, eniToIP map[string]string, cfg types.Config) (map[string]*model.Service, map[string][]*model.ServiceInstance) {
	services := make(map[string]*model.Service)
	instances := make(map[string][]*model.ServiceInstance)

	for _, t := range tasks {
		// determine IP
		ip := taskPrivateIP(t, eniToIP)
		if ip == "" {
			continue
		}

		// loop containers
		for _, c := range t.Containers {
			for _, mp := range c.NetworkBindings {
				// ECS gives host/container ports; we care about container port
				if mp.ContainerPort == nil {
					continue
				}
				port := int(*mp.ContainerPort)

				// build service key
				svcName := serviceNameForTask(&t, &c)
				hostname := host.Name(fmt.Sprintf("%s.ecs.local", svcName))

				// ensure service entry exists
				if _, f := services[svcName]; !f {
					services[svcName] = &model.Service{
						Hostname:   hostname,
						Resolution: model.Passthrough, // ECS has no native DNS in Istio
						Ports:      []*model.Port{{Name: fmt.Sprintf("tcp-%d", port), Port: port, Protocol: "TCP"}},
						Attributes: model.ServiceAttributes{
							Name:      svcName,
							Namespace: "ecs", // pseudo-namespace
						},
					}
				}

				// build instance
				inst := &model.ServiceInstance{
					Service: services[svcName],
					ServicePort: &model.Port{
						Name:     fmt.Sprintf("tcp-%d", port),
						Port:     port,
						Protocol: "TCP",
					},
					Endpoint: &model.IstioEndpoint{
						Addresses:       []string{ip},
						EndpointPort:    uint32(port),
						ServicePortName: fmt.Sprintf("tcp-%d", port),
						Labels:          extractLabels(&t, &c, cfg),
					},
				}

				instances[svcName] = append(instances[svcName], inst)
			}
		}
	}

	return services, instances
}

// taskPrivateIP resolves a private IP for a task.
func taskPrivateIP(t ecst.Task, eniToIP map[string]string) string {
	// check attachments
	for _, att := range t.Attachments {
		for _, d := range att.Details {
			if d.Name != nil && *d.Name == "privateIPv4Address" && d.Value != nil {
				return *d.Value
			}
			if d.Name != nil && *d.Name == "networkInterfaceId" && d.Value != nil {
				if ip, ok := eniToIP[*d.Value]; ok {
					return ip
				}
			}
		}
	}
	// fallback: container networkInterfaces
	for _, c := range t.Containers {
		for _, ni := range c.NetworkInterfaces {
			if ni.PrivateIpv4Address != nil {
				return *ni.PrivateIpv4Address
			}
		}
	}
	return ""
}

// serviceNameForTask creates a stable name for ECS service/task.
func serviceNameForTask(t *ecst.Task, c *ecst.Container) string {
	// prefer ECS service name
	if t.Group != nil && strings.HasPrefix(*t.Group, "service:") {
		return strings.TrimPrefix(*t.Group, "service:")
	}
	// fallback: task definition family or container name
	if t.TaskDefinitionArn != nil {
		parts := strings.Split(*t.TaskDefinitionArn, "/")
		if len(parts) > 1 {
			return parts[len(parts)-1]
		}
	}
	if c.Name != nil {
		return *c.Name
	}
	return "ecs-task"
}

// extractLabels builds Istio labels for the workload.
func extractLabels(t *ecst.Task, c *ecst.Container, cfg types.Config) labels.Instance {
	l := labels.Instance{}
	if t.LaunchType != "" {
		l["ecs.launchType"] = string(t.LaunchType)
	}
	if t.TaskArn != nil {
		l["ecs.taskArn"] = *t.TaskArn
	}
	if t.TaskDefinitionArn != nil {
		l["ecs.taskDefArn"] = *t.TaskDefinitionArn
	}
	if c.Name != nil {
		l["ecs.container"] = *c.Name
	}
	l["ecs.cluster"] = cfg.ClusterName
	l["ecs.region"] = cfg.Region
	return l
}
