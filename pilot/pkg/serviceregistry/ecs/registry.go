package ecs

import (
	"context"
	"sync"
	"time"

	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"istio.io/istio/pilot/pkg/model"
	ecsutil "istio.io/istio/pilot/pkg/serviceregistry/ecs/types"
	"istio.io/istio/pilot/pkg/serviceregistry/provider"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/util/sets"
)

var scope = log.RegisterScope("ecs-registry", "ECS service registry")

// Controller implements Istio's ServiceDiscovery + Controller primitives for ECS.
type Controller struct {
	cfg ecsutil.Config
	ecs *ecs.Client
	ec2 *ec2.Client

	// cache
	mu        sync.RWMutex
	services  map[string]*model.Service           // svcKey -> Service
	instances map[string][]*model.ServiceInstance // svcKey -> instances

	// stop
	cancelFunc context.CancelFunc
}

func NewController(cfg ecsutil.Config) (*Controller, error) {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 10 * time.Second
	}
	ctx := context.Background()
	awsCfg, err := awscfg.LoadDefaultConfig(ctx, awscfg.WithRegion(cfg.Region))
	if err != nil {
		return nil, err
	}
	c := &Controller{
		cfg:       cfg,
		ecs:       ecs.NewFromConfig(awsCfg),
		ec2:       ec2.NewFromConfig(awsCfg),
		services:  make(map[string]*model.Service),
		instances: make(map[string][]*model.ServiceInstance),
	}
	return c, nil
}

func (c *Controller) Run(stop <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancelFunc = cancel
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()

	// initial sync
	c.syncOnce(ctx)

	for {
		select {
		case <-ticker.C:
			c.syncOnce(ctx)
		case <-stop:
			scope.Info("ECS registry stopping")
			return
		}
	}
}

func (c *Controller) syncOnce(ctx context.Context) {
	scope.Debug("ECS registry sync start")

	taskArns, err := listClusterTasks(ctx, c.ecs, c.cfg.ClusterName)
	if err != nil {
		scope.Errorf("list tasks: %v", err)
		return
	}
	if len(taskArns) == 0 {
		c.setState(nil, nil)
		return
	}

	dresp, err := c.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: &c.cfg.ClusterName,
		Tasks:   taskArns,
		Include: []types.TaskField{types.TaskFieldTags},
	})
	if err != nil {
		scope.Errorf("describe tasks: %v", err)
		return
	}

	eniIDs := CollectENIIDs(dresp.Tasks)
	eniToIP := map[string]string{}
	if c.cfg.EnableENIQuery && len(eniIDs) > 0 {
		eniToIP, err = DescribeENIs(ctx, c.ec2, eniIDs)
		if err != nil {
			scope.Warnf("describe ENIs failed, continuing with empty map: %v", err)
		}
	}

	// Build services and instances
	services, instances := BuildModelFromTasks(dresp.Tasks, eniToIP, c.cfg)
	c.setState(services, instances)
}

func (c *Controller) setState(svcs map[string]*model.Service, inst map[string][]*model.ServiceInstance) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if svcs == nil {
		c.services = map[string]*model.Service{}
		c.instances = map[string][]*model.ServiceInstance{}
		return
	}
	c.services = svcs
	c.instances = inst
}

// --- Istio ServiceDiscovery surface ---

func (c *Controller) Services() []*model.Service {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*model.Service, 0, len(c.services))
	for _, s := range c.services {
		out = append(out, s)
	}
	return out
}

func (c *Controller) GetService(hostname host.Name) *model.Service {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.services[string(hostname)]; ok {
		return s
	}
	return nil
}

func (c *Controller) InstancesByPort(svc *model.Service, port int, labels labels.Instance) ([]*model.ServiceInstance, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	key := string(svc.Hostname)
	return c.instances[key], nil
}
func (c *Controller) GetProxyServiceInstances(*model.Proxy) ([]*model.ServiceInstance, error) {
	return nil, nil
}
func (c *Controller) GetProxyWorkloadLabels(proxy *model.Proxy) labels.Instance {
	return labels.Instance{}
}
func (c *Controller) AdditionalPodSubscriptions(proxy *model.Proxy, pushServices sets.String, proxyServices sets.String) sets.String {
	// If ECS doesn't require extra subscriptions, just return empty set
	return sets.New[string]()
}
func (c *Controller) AddressInformation(services sets.String) ([]model.AddressInfo, sets.String) {
	return []model.AddressInfo{}, sets.New[string]()
}
func (c *Controller) AppendServiceHandler(f model.ServiceHandler) {
	// no-op
}
func (c *Controller) AppendInstanceHandler(func(*model.ServiceInstance, model.Event)) {}
func (c *Controller) AppendNetworkGatewayHandler(f func()) {
	// no-op
}
func (c *Controller) AppendWorkloadHandler(f func(*model.WorkloadInstance, model.Event)) {
	// no-op
}
func (c *Controller) GetProxyServiceTargets(proxy *model.Proxy) []model.ServiceTarget {
	// Return an empty slice or implement actual logic as required.
	return []model.ServiceTarget{}
}
func (c *Controller) HasSynced() bool {
	// If you track initialization, return true only when ECS tasks are loaded.
	return true
}
func (c *Controller) MCSServices() []model.MCSServiceInfo {
	return []model.MCSServiceInfo{}
}
func (c *Controller) NetworkGateways() []model.NetworkGateway {
	return []model.NetworkGateway{}
}

// Policies implements serviceregistry.Instance
func (c *Controller) Policies(keys sets.Set[model.ConfigKey]) []model.WorkloadAuthorization {
	// ECS does not have policies yet, return empty slice
	return []model.WorkloadAuthorization{}
}
func (c *Controller) Provider() provider.ID {
	return provider.ID("ECS")
}
func (c *Controller) ServicesForWaypoint(key model.WaypointKey) []model.ServiceInfo {
	// ECS does not have waypoint proxies yet, return empty slice
	return []model.ServiceInfo{}
}
func (c *Controller) ServicesWithWaypoint(key string) []model.ServiceWaypointInfo {
	// ECS does not have waypoints, return empty slice
	return []model.ServiceWaypointInfo{}
}
func (c *Controller) WorkloadsForWaypoint(key model.WaypointKey) []model.WorkloadInfo {
	// ECS does not have waypoint workloads, return empty slice
	return []model.WorkloadInfo{}
}

// Cluster implements ClusterIDProvider when present.
func (c *Controller) Cluster() cluster.ID { return cluster.ID("ecs") }

// Stop allows explicit shutdown.
func (c *Controller) Stop() {
	if c.cancelFunc != nil {
		c.cancelFunc()
	}
}

// Helpers
func listClusterTasks(ctx context.Context, cli *ecs.Client, cluster string) ([]string, error) {
	out := []string{}
	paginator := ecs.NewListTasksPaginator(cli, &ecs.ListTasksInput{Cluster: &cluster})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, page.TaskArns...)
	}
	return out, nil
}
