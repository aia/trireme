package supervisor

import (
	"fmt"
	"strconv"

	"go.uber.org/zap"

	"github.com/aporeto-inc/trireme/cache"
	"github.com/aporeto-inc/trireme/collector"
	"github.com/aporeto-inc/trireme/constants"
	"github.com/aporeto-inc/trireme/enforcer"
	"github.com/aporeto-inc/trireme/monitor/linuxmonitor/cgnetcls"
	"github.com/aporeto-inc/trireme/policy"
	"github.com/aporeto-inc/trireme/supervisor/ipsetctrl"
	"github.com/aporeto-inc/trireme/supervisor/iptablesctrl"
)

type cacheData struct {
	version int
	ips     *policy.IPMap
	mark    string
	port    string
}

// Config is the structure holding all information about the supervisor
type Config struct {
	//implType ImplementationType
	mode constants.ModeType

	versionTracker cache.DataStore
	collector      collector.EventCollector

	networkQueues     string
	applicationQueues string

	Mark        int
	excludedIPs []string
	impl        Implementor
}

// NewSupervisor will create a new connection supervisor that uses IPTables
// to redirect specific packets to userspace. It instantiates multiple data stores
// to maintain efficient mappings between contextID, policy and IP addresses. This
// simplifies the lookup operations at the expense of memory.
func NewSupervisor(collector collector.EventCollector, enforcerInstance enforcer.PolicyEnforcer, mode constants.ModeType, implementation constants.ImplementationType) (*Config, error) {

	if collector == nil {
		return nil, fmt.Errorf("Collector cannot be nil")
	}

	if enforcerInstance == nil {
		return nil, fmt.Errorf("Enforcer cannot be nil")
	}

	filterQueue := enforcerInstance.GetFilterQueue()

	if filterQueue == nil {
		return nil, fmt.Errorf("Enforcer FilterQueues cannot be nil")
	}

	s := &Config{
		mode:              mode,
		impl:              nil,
		versionTracker:    cache.NewCache(),
		collector:         collector,
		networkQueues:     strconv.Itoa(int(filterQueue.NetworkQueue)) + ":" + strconv.Itoa(int(filterQueue.NetworkQueue+filterQueue.NumberOfNetworkQueues-1)),
		applicationQueues: strconv.Itoa(int(filterQueue.ApplicationQueue)) + ":" + strconv.Itoa(int(filterQueue.ApplicationQueue+filterQueue.NumberOfApplicationQueues-1)),
		Mark:              filterQueue.MarkValue,
		excludedIPs:       []string{},
	}

	var err error
	switch implementation {
	case constants.IPSets:
		s.impl, err = ipsetctrl.NewInstance(s.networkQueues, s.applicationQueues, s.Mark, false, mode)
	default:
		s.impl, err = iptablesctrl.NewInstance(s.networkQueues, s.applicationQueues, s.Mark, mode)
	}

	if err != nil {
		return nil, fmt.Errorf("Unable to initialize supervisor controllers")
	}

	return s, nil
}

// Supervise creates a mapping between an IP address and the corresponding labels.
// it invokes the various handlers that process the parameter policy.
func (s *Config) Supervise(contextID string, containerInfo *policy.PUInfo) error {

	if containerInfo == nil || containerInfo.Policy == nil || containerInfo.Runtime == nil {
		return fmt.Errorf("Runtime, Policy and ContainerInfo should not be nil")
	}

	_, err := s.versionTracker.Get(contextID)

	if err != nil {
		// ContextID is not found in Cache, New PU: Do create.
		return s.doCreatePU(contextID, containerInfo)
	}

	// Context already in the cache. Just run update
	return s.doUpdatePU(contextID, containerInfo)
}

// Unsupervise removes the mapping from cache and cleans up the iptable rules. ALL
// remove operations will print errors by they don't return error. We want to force
// as much cleanup as possible to avoid stale state
func (s *Config) Unsupervise(contextID string) error {

	version, err := s.versionTracker.Get(contextID)

	if err != nil {
		return fmt.Errorf("Cannot find policy version")
	}

	cacheEntry := version.(*cacheData)

	if err := s.impl.DeleteRules(cacheEntry.version, contextID, cacheEntry.ips, cacheEntry.port, cacheEntry.mark); err != nil {
		zap.L().Warn("Some rules were not deleted during unsupervise", zap.Error(err))
	}

	if err := s.versionTracker.Remove(contextID); err != nil {
		zap.L().Warn("Failed to clean the rule version cache", zap.Error(err))
	}

	return nil
}

// Start starts the supervisor
func (s *Config) Start() error {

	if err := s.impl.Start(); err != nil {
		return fmt.Errorf("Filter of marked packets was not set")
	}

	zap.L().Debug("Started the supervisor")

	return nil
}

// Stop stops the supervisor
func (s *Config) Stop() error {

	if err := s.impl.Stop(); err != nil {
		return fmt.Errorf("Failed to stop the implementer: %s", err)
	}

	return nil
}

func (s *Config) doCreatePU(contextID string, containerInfo *policy.PUInfo) error {

	zap.L().Debug("IPTables update for the creation of a pu", zap.String("contextID", contextID))

	version := 0
	mark, _ := containerInfo.Runtime.Options().Get(cgnetcls.CgroupMarkTag)
	port, ok := containerInfo.Runtime.Options().Get(cgnetcls.PortTag)
	if !ok {
		port = "0"
	}
	cacheEntry := &cacheData{
		version: version,
		ips:     containerInfo.Policy.IPAddresses(),
		mark:    mark,
		port:    port,
	}

	// Version the policy so that we can do hitless policy changes
	s.versionTracker.AddOrUpdate(contextID, cacheEntry)

	if err := s.impl.ConfigureRules(version, contextID, containerInfo); err != nil {
		if uerr := s.Unsupervise(contextID); uerr != nil {
			zap.L().Warn("Failed to clean up state while creating the PU",
				zap.String("contextID", contextID),
				zap.Error(uerr),
			)
		}
		return err
	}

	return nil
}

// UpdatePU creates a mapping between an IP address and the corresponding labels
//and the invokes the various handlers that process all policies.
func (s *Config) doUpdatePU(contextID string, containerInfo *policy.PUInfo) error {

	cacheEntry, err := s.versionTracker.LockedModify(contextID, add, 1)

	if err != nil {
		return fmt.Errorf("Error finding PU in cache %s", err)
	}

	cachedEntry := cacheEntry.(*cacheData)

	if err := s.impl.UpdateRules(cachedEntry.version, contextID, containerInfo); err != nil {
		if uerr := s.Unsupervise(contextID); uerr != nil {
			zap.L().Warn("Failed to clean up state while updating the PU",
				zap.String("contextID", contextID),
				zap.Error(uerr),
			)
		}
		return err
	}

	return nil
}

func add(a, b interface{}) interface{} {
	entry := a.(*cacheData)
	entry.version += b.(int)
	return entry
}
