//go:build linux && podman
// +build linux,podman

package podman

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	netTypes "github.com/containers/common/libnetwork/types"
	"github.com/containers/podman/v4/pkg/bindings/containers"
	"github.com/containers/podman/v4/pkg/domain/entities"
	"github.com/containers/podman/v4/pkg/specgen"
	"github.com/dustin/go-humanize"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containers/podman/v4/pkg/bindings"
	"github.com/google/shlex"
	log "github.com/sirupsen/logrus"
	"github.com/srl-labs/containerlab/types"
	"github.com/srl-labs/containerlab/utils"
)

var errInvalidBind = errors.New("invalid bind mount provided")

type podmanWriterCloser struct {
	bytes.Buffer
}

func (*podmanWriterCloser) Close() error {
	return nil
}

func (*PodmanRuntime) connect(ctx context.Context) (context.Context, error) {
	return bindings.NewConnection(ctx, "unix://run/podman/podman.sock")
}

func (r *PodmanRuntime) createContainerSpec(ctx context.Context, cfg *types.NodeConfig) (specgen.SpecGenerator, error) {
	sg := specgen.SpecGenerator{}
	cmd, err := shlex.Split(cfg.Cmd)
	if err != nil {
		return sg, err
	}
	entrypoint, err := shlex.Split(cfg.Entrypoint)
	if err != nil {
		return sg, err
	}
	// Main container specs
	labels := cfg.Labels
	// encode a mgmt net name as an extra label
	labels["clab-net-mgmt"] = r.mgmt.Network
	specBasicConfig := specgen.ContainerBasicConfig{
		Name:       cfg.LongName,
		Entrypoint: entrypoint,
		Command:    cmd,
		EnvHost:    false,
		HTTPProxy:  false,
		Env:        cfg.Env,
		Terminal:   false,
		Stdin:      false,
		Labels:     cfg.Labels,
		Hostname:   cfg.ShortName,
		Sysctl:     cfg.Sysctls,
		Remove:     false,
	}
	// Storage, image and mounts
	mounts, err := r.convertMounts(ctx, cfg.Binds)
	if err != nil {
		log.Errorf("Cannot convert mounts %v: %v", cfg.Binds, err)
		mounts = nil
	}
	specStorageConfig := specgen.ContainerStorageConfig{
		Image: cfg.Image,
		// Rootfs:            "",
		// ImageVolumeMode:   "",
		// VolumesFrom:       nil,
		// Init:              false,
		// InitPath:          "",
		Mounts: mounts,
		// Volumes:           nil,
		// OverlayVolumes:    nil,
		// ImageVolumes:      nil,
		// Devices:           nil,
		// DeviceCGroupRule:  nil,
		// IpcNS:             specgen.Namespace{},
		// ShmSize:           nil,
		// WorkDir:           "",
		// RootfsPropagation: "",
		// Secrets:           nil,
		// Volatile:          false,
	}
	// Security
	specSecurityConfig := specgen.ContainerSecurityConfig{
		Privileged: true,
		User:       cfg.User,
	}
	// Going with the defaults for cgroups
	specCgroupConfig := specgen.ContainerCgroupConfig{
		CgroupNS: specgen.Namespace{},
	}
	// Resource limits
	var (
		resLimits specs.LinuxResources
		lMem      specs.LinuxMemory
		lCPU      specs.LinuxCPU
	)
	// Memory limits
	if cfg.Memory != "" {
		mem, err := humanize.ParseBytes(cfg.Memory)
		mem64 := int64(mem)
		if err != nil {
			log.Warnf("Unable to parse memory limit %q for node %q", cfg.Memory, cfg.LongName)
		}
		lMem.Limit = &mem64
	}
	resLimits.Memory = &lMem
	// CPU resources limits
	if cfg.CPU != 0 {
		quota := int64(cfg.CPU * 100000)
		lCPU.Quota = &quota
		period := uint64(100000)
		lCPU.Period = &period
	}
	if cfg.CPUSet != "" {
		lCPU.Cpus = cfg.CPUSet
	}
	resLimits.CPU = &lCPU

	specResConfig := specgen.ContainerResourceConfig{
		ResourceLimits: &resLimits,
		// Rlimits:                 nil,
		// OOMScoreAdj:             nil,
		// WeightDevice:            nil,
		// ThrottleReadBpsDevice:   nil,
		// ThrottleWriteBpsDevice:  nil,
		// ThrottleReadIOPSDevice:  nil,
		// ThrottleWriteIOPSDevice: nil,
		// CgroupConf:              nil,
		// CPUPeriod:               0,
		// CPUQuota:                0,
	}
	// Defaults for health checks
	specHCheckConfig := specgen.ContainerHealthCheckConfig{}
	// Everything below is related to network spec of a container
	specNetConfig := specgen.ContainerNetworkConfig{}

	netMode := strings.SplitN(cfg.NetworkMode, ":", 2)
	switch netMode[0] {
	case "container":
		// We expect exactly two arguments in this case ("container" keyword & cont. name/ID)
		if len(netMode) != 2 {
			return sg, fmt.Errorf("container network mode was specified for container %q, but no container name was found: %q", cfg.ShortName, netMode)
		}
		// also cont. ID shouldn't be empty
		if netMode[1] == "" {
			return sg, fmt.Errorf("container network mode was specified for container %q, but no container name was found: %q", cfg.ShortName, netMode)
		}
		// Extract lab/topo prefix to provide a full (long) container name. Hackish way.
		prefix := strings.SplitN(cfg.LongName, cfg.ShortName, 2)[0]
		// Compile the net spec
		specNetConfig = specgen.ContainerNetworkConfig{
			NetNS: specgen.Namespace{
				NSMode: "container",
				Value:  prefix + netMode[1],
			},
		}
	case "host":
		specNetConfig = specgen.ContainerNetworkConfig{
			NetNS: specgen.Namespace{NSMode: "host"},
			// UseImageResolvConf:  false,
			// DNSServers:          nil,
			// DNSSearch:           nil,
			// DNSOptions:          nil,
			UseImageHosts: false,
			HostAdd:       cfg.ExtraHosts,
			// NetworkOptions:      nil,
		}
	// Bridge will be used if none provided
	case "bridge", "":
		netName := r.mgmt.Network
		mac, err := net.ParseMAC(cfg.MacAddress)
		if err != nil && cfg.MacAddress != "" {
			return sg, err
		}
		// Podman uses a custom type for mac addresses, so we need to convert it first
		hwAddr := netTypes.HardwareAddr(mac)
		staticIPs := make([]net.IP, 0)
		if mgmtv4Addr := net.ParseIP(cfg.MgmtIPv4Address); mgmtv4Addr != nil {
			staticIPs = append(staticIPs, mgmtv4Addr)
		}
		if mgmtv6Addr := net.ParseIP(cfg.MgmtIPv6Address); mgmtv6Addr != nil {
			staticIPs = append(staticIPs, mgmtv6Addr)
		}
		// Static IPs & Macs are properties of a network attachment
		nets := map[string]netTypes.PerNetworkOptions{netName: {
			StaticIPs:     staticIPs,
			Aliases:       nil,
			StaticMAC:     hwAddr,
			InterfaceName: "",
		}}
		portmap, err := r.convertPortMap(ctx, cfg.PortBindings)
		if err != nil {
			return sg, err
		}
		expose, err := r.convertExpose(ctx, cfg.PortSet)
		if err != nil {
			return sg, err
		}
		specNetConfig = specgen.ContainerNetworkConfig{
			NetNS:               specgen.Namespace{NSMode: "bridge"},
			PortMappings:        portmap,
			PublishExposedPorts: false,
			Expose:              expose,
			Networks:            nets,
			// UseImageResolvConf:  false,
			// DNSServers:          nil,
			// DNSSearch:           nil,
			// DNSOptions:          nil,
			UseImageHosts: false,
			HostAdd:       cfg.ExtraHosts,
			// NetworkOptions:      nil,
		}
	default:
		return sg, fmt.Errorf("network Mode %q is not currently supported with Podman", netMode)
	}
	// Compile the final spec
	sg = specgen.SpecGenerator{
		ContainerBasicConfig:       specBasicConfig,
		ContainerStorageConfig:     specStorageConfig,
		ContainerSecurityConfig:    specSecurityConfig,
		ContainerCgroupConfig:      specCgroupConfig,
		ContainerNetworkConfig:     specNetConfig,
		ContainerResourceConfig:    specResConfig,
		ContainerHealthCheckConfig: specHCheckConfig,
	}
	return sg, nil
}

// convertMounts takes a list of filesystem mount binds in docker/clab format (src:dest:options)
// and converts it into an opencontainers spec format.
func (*PodmanRuntime) convertMounts(_ context.Context, mounts []string) ([]specs.Mount, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	mntSpec := make([]specs.Mount, len(mounts))
	// Note: we don't do any input validation here
	for i, mnt := range mounts {
		mntSplit := strings.SplitN(mnt, ":", 3)

		if len(mntSplit) == 1 {
			return nil, fmt.Errorf("%w: %s", errInvalidBind, mnt)
		}

		mntSpec[i] = specs.Mount{
			Destination: mntSplit[1],
			Type:        "bind",
			Source:      mntSplit[0],
		}

		// when options are provided in the bind mount spec
		if len(mntSplit) == 3 {
			mntSpec[i].Options = strings.Split(mntSplit[2], ",")
		}
	}
	log.Debugf("convertMounts method received mounts %v and produced %+v as a result", mounts, mntSpec)
	return mntSpec, nil
}

// produceGenericContainerList takes a list of containers in a podman entities.ListContainer format
// and transforms it into a GenericContainer type.
func (r *PodmanRuntime) produceGenericContainerList(ctx context.Context,
	cList []entities.ListContainer,
) ([]types.GenericContainer, error) {
	genericList := make([]types.GenericContainer, len(cList))
	for i, v := range cList {
		netSettings, err := r.extractMgmtIP(ctx, v.ID)
		if err != nil {
			return nil, err
		}
		genericList[i] = types.GenericContainer{
			Names:           v.Names,
			ID:              v.ID,
			ShortID:         v.ID[:12],
			Image:           v.Image,
			State:           v.State,
			Status:          v.Status,
			Labels:          v.Labels,
			Pid:             v.Pid,
			NetworkSettings: netSettings,
		}
	}
	log.Debugf("Method produceGenericContainerList returns %+v", genericList)
	return genericList, nil
}

func (*PodmanRuntime) extractMgmtIP(ctx context.Context, cID string) (types.GenericMgmtIPs, error) {
	// First get all the data from the inspect
	toReturn := types.GenericMgmtIPs{}
	inspectRes, err := containers.Inspect(ctx, cID, &containers.InspectOptions{})
	if err != nil {
		log.Debugf("Couldn't extract mgmt IPs for container %q, %v", cID, err)
	}
	// Extract the data only for a specific CNI. Network name is taken from a container's label
	netName, ok := inspectRes.Config.Labels["clab-net-mgmt"]
	if !ok || netName == "" {
		log.Debugf("Couldn't extract mgmt net data for container %q", cID)
		return toReturn, nil
	}
	mgmtData, ok := inspectRes.NetworkSettings.Networks[netName]
	if !ok || mgmtData == nil {
		log.Debugf("Couldn't extract mgmt IPs for container %q and net %q", cID, netName)
		return toReturn, nil
	}
	log.Debugf("extractMgmtIPs was called and we got a struct %T %+v", mgmtData, mgmtData)
	v4addr := mgmtData.IPAddress
	v4pLen := mgmtData.IPPrefixLen
	v4Gw := mgmtData.Gateway
	v6addr := mgmtData.GlobalIPv6Address
	v6pLen := mgmtData.GlobalIPv6PrefixLen

	toReturn = types.GenericMgmtIPs{
		IPv4addr: v4addr,
		IPv4pLen: v4pLen,
		IPv6addr: v6addr,
		IPv6pLen: v6pLen,
		IPv4Gw:   v4Gw,
	}
	return toReturn, nil
}

func (r *PodmanRuntime) disableTXOffload(_ context.Context) error {
	// TX checksum disabling will be done here since the mgmt bridge
	// may not exist in netlink before a container is attached to it
	brName := r.mgmt.Bridge
	log.Debugf("Got a bridge name %q", brName)
	// Disable checksum calculation hw offload
	err := utils.EthtoolTXOff(brName)
	if err != nil {
		log.Warnf("failed to disable TX checksum offload for interface %q: %v", brName, err)
		return err
	}
	log.Debugf("Successully disabled Tx checksum offload for interface %q", brName)
	return nil
}

// netOpts is an accessory function that returns a network.CreateOptions struct
// filled with all parameters for CreateNet function.
func (r *PodmanRuntime) netOpts(_ context.Context) (netTypes.Network, error) {
	// set bridge name = network name if explicit name was not provided
	if r.mgmt.Bridge == "" && r.mgmt.Network != "" {
		r.mgmt.Bridge = r.mgmt.Network
	}
	var (
		name        = r.mgmt.Network
		intName     = r.mgmt.Bridge
		driver      = "bridge"
		internal    = false
		ipv6        = false
		dnsEnabled  = false
		options     = map[string]string{}
		labels      = map[string]string{"containerlab": ""}
		err         error
		ipamOptions = map[string]string{}
		v4subnet    = netTypes.Subnet{}
		v6subnet    = netTypes.Subnet{}
		subnets     = make([]netTypes.Subnet, 0)
	)
	// parse mgmt subnets
	// check if v4 is defined
	if r.mgmt.IPv4Subnet != "" {
		v4subnet.Subnet, err = netTypes.ParseCIDR(r.mgmt.IPv4Subnet)
		if err != nil {
			return netTypes.Network{}, err
		}
		// add a custom gw address if specified
		if r.mgmt.IPv4Gw != "" && r.mgmt.IPv4Gw != "0.0.0.0" {
			v4subnet.Gateway = net.ParseIP(r.mgmt.IPv4Gw)
		}
		subnets = append(subnets, v4subnet)
		log.Debugf("Added v4 subnet info to the net definion: \n%v, \n%v\n", subnets, v4subnet)
	}
	// check if v6 is defined
	if r.mgmt.IPv6Subnet != "" {
		v6subnet.Subnet, err = netTypes.ParseCIDR(r.mgmt.IPv6Subnet)
		if err != nil {
			return netTypes.Network{}, err
		}
		ipv6 = true
		// add a custom gw address if specified
		if r.mgmt.IPv6Gw != "" && r.mgmt.IPv6Gw != "::" {
			v6subnet.Gateway = net.ParseIP(r.mgmt.IPv6Gw)
		}
		subnets = append(subnets, v6subnet)
		log.Debugf("Added v6 subnet info to the net definion: \n%v, \n%v\n", subnets, v6subnet)
	}

	// add custom mtu if defined
	if r.mgmt.MTU != "" {
		options["mtu"] = r.mgmt.MTU
	}
	// compile the resulting struct
	toReturn := netTypes.Network{
		DNSEnabled:       dnsEnabled,
		Driver:           driver,
		Internal:         internal,
		Labels:           labels,
		Subnets:          subnets,
		IPv6Enabled:      ipv6,
		Options:          options,
		Name:             name,
		IPAMOptions:      ipamOptions,
		NetworkInterface: intName,
	}
	return toReturn, nil
}

func (*PodmanRuntime) buildFilterString(gFilters []*types.GenericFilter) map[string][]string {
	filters := map[string][]string{}
	for _, gF := range gFilters {
		filterType := gF.FilterType
		filterOp := gF.Operator
		filterValue := gF.Match
		if filterOp == "exists" {
			filterOp = "="
			filterValue = ""
		}
		filterStr := ""
		if filterType == "name" {
			filterStr = filterValue
		} else if filterOp != "=" {
			log.Warnf("received a filter with unsupported match type: %+v", gF)
			continue
		} else {
			filterStr = gF.Field + filterOp + filterValue
		}
		log.Debugf("produced a filterStr %q from inputs %+v", filterStr, gF)
		_, ok := filters[filterType]
		if !ok {
			filters[filterType] = []string{}
		}
		filters[filterType] = append(filters[filterType], filterStr)
	}
	log.Debugf("Method buildFilterString was called with inputs %+v\n and results %+v", gFilters, filters)
	return filters
}

// postStartActions performs misc. tasks that are needed after the container starts.
func (r *PodmanRuntime) postStartActions(ctx context.Context, cID string, cfg *types.NodeConfig) error {
	var err error
	// Add NSpath to the node config struct
	cfg.NSPath, err = r.GetNSPath(ctx, cID)
	if err != nil {
		return err
	}
	// And setup netns alias. Not really needed with podman
	// But currently (Oct 2021) clab depends on the specific naming scheme of veth aliases.
	err = utils.LinkContainerNS(cfg.NSPath, cfg.LongName)
	if err != nil {
		return err
	}
	// TX checksum disabling will be done here since the mgmt bridge
	// may not exist in netlink before a container is attached to it
	err = r.disableTXOffload(ctx)
	return err
}
