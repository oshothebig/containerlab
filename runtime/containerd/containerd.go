package containerd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/docker/go-units"
	"github.com/google/shlex"
	"github.com/opencontainers/runtime-spec/specs-go"
	log "github.com/sirupsen/logrus"
	"github.com/srl-labs/containerlab/types"
	"github.com/srl-labs/containerlab/utils"
)

const (
	containerdNamespace = "clab"

	cniBin   = "/opt/cni/bin"
	cniCache = "/opt/cni/cache"
)

type ContainerdRuntime struct {
	client           *containerd.Client
	timeout          time.Duration
	Mgmt             types.MgmtNet
	debug            bool
	gracefulShutdown bool
}

func NewContainerdRuntime(d bool, dur time.Duration, gracefulShutdown bool) *ContainerdRuntime {
	c, err := containerd.New("/run/containerd/containerd.sock")
	if err != nil {
		log.Fatalf("failed to create containerd client: %v", err)
	}

	return &ContainerdRuntime{
		client:           c,
		debug:            d,
		timeout:          dur,
		gracefulShutdown: gracefulShutdown,
	}
}

func (c *ContainerdRuntime) SetMgmtNet(n types.MgmtNet) {
	if n.Bridge == "" {
		netname := "clab"
		if n.Network == "" {
			netname = n.Network
		}
		n.Bridge = "br-" + netname
	}
	c.Mgmt = n
}

func (c *ContainerdRuntime) CreateNet(ctx context.Context) error {
	log.Debug("CreateNet() - Not needed with containerd")
	return nil
}
func (c *ContainerdRuntime) DeleteNet(context.Context) error {
	log.Debug("DeleteNet() - Not yet required with containerd")
	return nil
}

func (c *ContainerdRuntime) PullImageIfRequired(ctx context.Context, imagename string) error {

	canonicalimage := utils.GetCanonicalImageName(imagename)

	log.Debugf("Looking up %s container image", canonicalimage)
	ctx = namespaces.WithNamespace(ctx, containerdNamespace)
	images, err := c.client.ListImages(ctx)
	if err != nil {
		return err
	}

	// If Image doesn't exist, we need to pull it
	if len(images) > 0 {
		log.Debugf("Image %s present, skip pulling", canonicalimage)
		return nil
	}

	_, err = c.client.Pull(ctx, canonicalimage, containerd.WithPullUnpack)
	if err != nil {
		return err
	}
	return nil
}

func (c *ContainerdRuntime) CreateContainer(ctx context.Context, node *types.Node) error {
	ctx = namespaces.WithNamespace(ctx, containerdNamespace)

	img, err := c.client.GetImage(ctx, node.Image)
	if err != nil {
		return err
	}

	cmd, err := shlex.Split(node.Cmd)
	if err != nil {
		return err
	}

	mounts := make([]specs.Mount, len(node.Binds))

	for idx, mount := range node.Binds {
		s := strings.Split(mount, ":")

		m := specs.Mount{
			Source:      s[0],
			Destination: s[1],
			Options:     []string{"rbind", "rprivate"},
		}
		if len(mount) == 3 {
			m.Options = append(m.Options, strings.Split(s[2], ",")...)
		}
		mounts[idx] = m
	}

	//mounts = append(mounts, specs.Mount{Type: "cgroup", Source: "cgroup", Destination: "/sys/fs/cgroup", Options: []string{"ro", "nosuid", "noexec", "nodev"}})

	_ = cmd
	opts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithEnv(utils.ConvertEnvs(node.Env)),
		//oci.WithProcessArgs("bash"),
		oci.WithHostname(node.ShortName),
		WithSysctls(node.Sysctls),
		//oci.WithAllKnownCapabilities,
		oci.WithoutRunMount,
		oci.WithPrivileged,
		oci.WithHostLocaltime,
		oci.WithNamespacedCgroup(),
		oci.WithAllDevicesAllowed,
		oci.WithDefaultUnixDevices,
		//oci.WithHostDevices,
		oci.WithNewPrivileges,
	}
	if len(cmd) > 0 {
		opts = append(opts, oci.WithProcessArgs(cmd...))
	}
	if node.User != "" {
		opts = append(opts, oci.WithUser(node.User))
	}

	if len(mounts) > 0 {
		opts = append(opts, oci.WithMounts(mounts))
	}

	var cnic *libcni.CNIConfig
	var cncl *libcni.NetworkConfigList
	var cnirc *libcni.RuntimeConf

	switch node.NetworkMode {
	case "host":
		opts = append(opts,
			oci.WithHostNamespace(specs.NetworkNamespace),
			oci.WithHostHostsFile,
			oci.WithHostResolvconf)
	case "none":
		// Done!
	default:
		cnic, cncl, cnirc, err = cniInit(node.LongName, "eth0", c.Mgmt)
		if err != nil {
			return err
		}

		// set mac if defined in node
		if node.MacAddress != "" {
			cnirc.CapabilityArgs["mac"] = node.MacAddress
		}

		portmappings := []portMapping{}

		for contdatasl, hostdata := range node.PortBindings {
			//fmt.Printf("%+v", hostdata)
			//fmt.Printf("%+v", contdatasl)
			for _, x := range hostdata {
				hostport, err := strconv.Atoi(x.HostPort)
				if err != nil {
					return err
				}
				portmappings = append(portmappings, portMapping{HostPort: hostport, ContainerPort: contdatasl.Int(), Protocol: contdatasl.Proto()})
			}
		}
		if len(portmappings) > 0 {
			cnirc.CapabilityArgs["portMappings"] = portmappings
		}

	}

	var cOpts []containerd.NewContainerOpts
	cOpts = append(cOpts,
		containerd.WithImage(img),
		containerd.WithNewSnapshot(node.LongName+"-snapshot", img),
		containerd.WithAdditionalContainerLabels(node.Labels),
		containerd.WithNewSpec(opts...),
	)

	newContainer, err := c.client.NewContainer(
		ctx,
		node.LongName,
		cOpts...,
	)
	if err != nil {
		return err
	}

	log.Debugf("Container '%s' created", node.LongName)
	log.Debugf("Start container: %s", node.LongName)

	err = c.StartContainer(ctx, node.LongName)
	if err != nil {
		return err
	}

	log.Debugf("Container started: %s", node.LongName)

	node.NSPath, err = c.GetNSPath(ctx, node.LongName)
	if err != nil {
		return err
	}

	err = utils.LinkContainerNS(node.NSPath, node.LongName)
	if err != nil {
		return err
	}

	// if this is not a host network namespace container
	// we have prepared a lot of stuff further up, which
	// is now to be applied
	if cnic != nil {
		cnirc.NetNS = node.NSPath
		res, err := cnic.AddNetworkList(ctx, cncl, cnirc)
		if err != nil {
			return err
		}
		result, _ := current.NewResultFromResult(res)

		ipv4, ipv6 := "", ""
		ipv4nm, ipv6nm := 0, 0
		for _, ip := range result.IPs {
			switch ip.Version {
			case "4":
				ipv4 = ip.Address.IP.String()
				ipv4nm, _ = ip.Address.Mask.Size()
			case "6":
				ipv6 = ip.Address.IP.String()
				ipv6nm, _ = ip.Address.Mask.Size()
			}
		}

		additionalLabels := map[string]string{
			"clab.ipv4.addr":    ipv4,
			"clab.ipv4.netmask": strconv.Itoa(ipv4nm),
			"clab.ipv6.addr":    ipv6,
			"clab.ipv6.netmask": strconv.Itoa(ipv6nm),
		}
		_, err = newContainer.SetLabels(ctx, additionalLabels)
		if err != nil {
			return err
		}
	}
	return nil
}

func cniInit(cId string, ifName string, mgmtNet types.MgmtNet) (*libcni.CNIConfig, *libcni.NetworkConfigList, *libcni.RuntimeConf, error) {
	// allow overwriting cni plugin binary path via ENV var
	cniPath, ok := os.LookupEnv("CNI_BIN")
	if !ok {
		cniPath = cniBin
	}

	cnic := libcni.NewCNIConfigWithCacheDir([]string{cniPath}, cniCache, nil)

	cniConfig := fmt.Sprintf(`
	{
		"cniVersion": "0.4.0",
		"name": "clabmgmt",
		"plugins": [
		  {
			"type": "bridge",
			"bridge": "%s",
			"isDefaultGateway": true,
			"forceAddress": false,
			"ipMasq": true,
			"hairpinMode": true,
			"ipam": {
			  "type": "host-local",
			  "ranges": [
				[
				  {
					"subnet": "%s"
				  }
				],
				[
				  {
					"subnet": "%s"
				  }
				]
			  ]
			}
		  },
		  {
			"type": "tuning",
			"mtu": %s,
			"capabilities": {
			  "mac": true
			}
		  },
		  {
			"type": "portmap",
			"capabilities": {
			  "portMappings": true
			}
		  }
		]
	  }
	`, mgmtNet.Bridge, mgmtNet.IPv4Subnet, mgmtNet.IPv6Subnet, mgmtNet.MTU)
	//log.Debug(cniConfig)
	cncl, err := libcni.ConfListFromBytes([]byte(cniConfig))
	if err != nil {
		return nil, nil, nil, err
	}

	cnirc := &libcni.RuntimeConf{
		ContainerID: cId,
		IfName:      ifName,
		//// NetNS must be set later, can just be determined after cotnainer start
		//NetNS:          node.NSPath,
		CapabilityArgs: make(map[string]interface{}),
	}
	return cnic, cncl, cnirc, nil
}

type portMapping struct {
	HostPort      int    `json:"hostPort"`
	HostIP        string `json:"hostIP,omitempty"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
}

func WithSysctls(sysctls map[string]string) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *specs.Spec) error {
		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		if s.Linux.Sysctl == nil {
			s.Linux.Sysctl = make(map[string]string)
		}
		for k, v := range sysctls {
			s.Linux.Sysctl[k] = v
		}
		return nil
	}
}

func (c *ContainerdRuntime) StartContainer(ctx context.Context, containername string) error {
	container, err := c.client.LoadContainer(ctx, containername)

	if err != nil {
		return err
	}
	task, err := container.NewTask(ctx, cio.LogFile("/tmp/clab/"+containername+".log"))
	if err != nil {
		log.Fatal(err)
		log.Fatalf("Failed to start container %s", containername)

		return err
	}
	err = task.Start(ctx)
	if err != nil {
		return err
	}
	return nil
}
func (c *ContainerdRuntime) StopContainer(ctx context.Context, containername string, dur *time.Duration) error {
	ctask, err := c.getContainerTask(ctx, containername)
	if err != nil {
		log.Debugf("no task found for container %s", containername)
		return nil
	}
	taskstatus, err := ctask.Status(ctx)
	if err != nil {
		return err
	}

	paused := false
	needsStop := true
	switch taskstatus.Status {
	case containerd.Created, containerd.Stopped:
		needsStop = false
	case containerd.Paused, containerd.Pausing:
		paused = true
	default:
	}

	if needsStop {
		// NOTE: ctx is main context so that it's ok to use for task.Wait().
		exitCh, err := ctask.Wait(ctx)
		if err != nil {
			return err
		}

		// signal will be sent once resume is finished
		if paused {
			if err := ctask.Resume(ctx); err != nil {
				log.Warnf("Cannot unpause container %s: %s", containername, err)
			} else {
				// no need to do it again when send sigkill signal
				paused = false
			}
		}

		err = ctask.Kill(ctx, syscall.SIGKILL)
		if err != nil {
			return err
		}

		err = waitContainerStop(ctx, exitCh, containername)
		if err != nil {
			return err
		}
	}

	existStatus, err := ctask.Delete(ctx)
	if err != nil {
		return err
	}
	log.Debugf("Container %s stopped with exit code %d", containername, existStatus.ExitCode())
	return nil
}

func waitContainerStop(ctx context.Context, exitCh <-chan containerd.ExitStatus, id string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case status := <-exitCh:
		return status.Error()
	}
}

func (c *ContainerdRuntime) getContainerTask(ctx context.Context, containername string) (containerd.Task, error) {
	ctx = namespaces.WithNamespace(ctx, containerdNamespace)
	cont, err := c.client.LoadContainer(ctx, containername)
	if err != nil {
		return nil, err
	}
	task, err := cont.Task(ctx, nil)
	if err != nil {
		return nil, err
	}
	return task, nil
}

func (c *ContainerdRuntime) ListContainers(ctx context.Context, filter []*types.GenericFilter) ([]types.GenericContainer, error) {
	log.Debug("listing containers")
	ctx = namespaces.WithNamespace(ctx, containerdNamespace)
	// TODO add containerlab label as filter criteria

	filterstring := c.filterStringBuilder(filter)

	containerlist, err := c.client.Containers(ctx, filterstring)
	if err != nil {
		return nil, err
	}

	return c.produceGenericContainerList(ctx, containerlist)
}

func (c *ContainerdRuntime) filterStringBuilder(filter []*types.GenericFilter) string {
	filterstring := ""
	delim := ""
	for _, filterEntry := range filter {
		isExistsOperator := false

		operator := filterEntry.Operator
		switch filterEntry.Operator {
		case "=":
			operator = "=="
		case "exists":
			operator = ""
			isExistsOperator = true
		}

		switch filterEntry.FilterType {
		case "label":
			filterstring = filterstring + "labels." + filterEntry.Field
			if !isExistsOperator {
				filterstring = filterstring + operator + filterEntry.Match + delim
			}

		}
		delim = ","
	}
	log.Debug("Filterstring: " + filterstring)
	return filterstring
}

// Transform docker-specific to generic container format
func (c *ContainerdRuntime) produceGenericContainerList(ctx context.Context, input []containerd.Container) ([]types.GenericContainer, error) {
	var result []types.GenericContainer

	for _, i := range input {

		ctr := types.GenericContainer{}

		info, err := i.Info(ctx)
		if err != nil {
			return nil, err
		}

		ctr.Names = []string{i.ID()}
		ctr.ID = i.ID()
		ctr.Image = info.Image
		ctr.Labels = info.Labels

		ctr.NetworkSettings, err = extractIPInfoFromLabels(ctr.Labels)
		if err != nil {
			return nil, err
		}

		taskfound := true
		task, err := i.Task(ctx, nil)
		if err != nil {
			// NOTE: NotFound doesn't mean that container hasn't started.
			// In docker/CRI-containerd plugin, the task will be deleted
			// when it exits. So, the status will be "created" for this
			// case.
			if errdefs.IsNotFound(err) {
				taskfound = false
			}
		}
		if taskfound {
			status, err := task.Status(ctx)
			if err != nil {
				log.Fatalf("failed to retrieve task status")
				return nil, err
			}
			ctr.State = string(status.Status)

			switch s := status.Status; s {
			case containerd.Stopped:
				ctr.Status = fmt.Sprintf("Exited (%v) %s", status.ExitStatus, timeSinceInHuman(status.ExitTime))
			case containerd.Running:
				ctr.Status = "Up"
			default:
				ctr.Status = strings.Title(string(s))
			}

			ctr.Pid = int(task.Pid())
		} else {
			ctr.State = strings.Title(string(containerd.Unknown))
			ctr.Status = "Unknown"
			ctr.Pid = -1
		}
		result = append(result, ctr)
	}
	return result, nil
}

func extractIPInfoFromLabels(labels map[string]string) (*types.GenericMgmtIPs, error) {
	var ipv4mask int
	var ipv6mask int
	var err error
	isSet := false
	if val, exists := labels["clab.ipv4.netmask"]; exists {
		isSet = true
		ipv4mask, err = strconv.Atoi(val)
		if err != nil {
			return nil, err
		}
	}
	if val, exists := labels["clab.ipv6.netmask"]; exists {
		isSet = true
		ipv6mask, err = strconv.Atoi(val)
		if err != nil {
			return nil, err
		}
	}
	return &types.GenericMgmtIPs{Set: isSet, IPv4addr: labels["clab.ipv4.addr"], IPv4pLen: ipv4mask, IPv6addr: labels["clab.ipv6.addr"], IPv6pLen: ipv6mask}, nil
}

func timeSinceInHuman(since time.Time) string {
	return units.HumanDuration(time.Since(since)) + " ago"
}

func (c *ContainerdRuntime) ContainerInspect(context.Context, string) (*types.GenericContainer, error) {
	log.Fatalf("ContainerInspect() - Not implemented yet")
	return &types.GenericContainer{}, nil
}
func (c *ContainerdRuntime) GetNSPath(ctx context.Context, containername string) (string, error) {
	ctx = namespaces.WithNamespace(ctx, containerdNamespace)
	task, err := c.getContainerTask(ctx, containername)
	if err != nil {
		return "", err
	}
	return "/proc/" + strconv.Itoa(int(task.Pid())) + "/ns/net", nil
}
func (c *ContainerdRuntime) Exec(context.Context, string, []string) ([]byte, []byte, error) {
	log.Fatalf("Exec() - Not implemented yet")
	return []byte(""), []byte(""), nil
}
func (c *ContainerdRuntime) ExecNotWait(context.Context, string, []string) error {
	log.Fatalf("ExecNotWait() - Not implemented yet")
	return nil
}
func (c *ContainerdRuntime) DeleteContainer(ctx context.Context, container *types.GenericContainer) error {
	log.Debugf("deleting container %s", container.ID)
	ctx = namespaces.WithNamespace(ctx, containerdNamespace)

	err := c.StopContainer(ctx, container.ID, nil)
	if err != nil {
		return err
	}

	cnic, cncl, cnirc, err := cniInit(container.ID, "eth0", c.Mgmt)
	if err != nil {
		return err
	}

	err = cnic.DelNetworkList(ctx, cncl, cnirc)
	if err != nil {
		return err
	}

	cont, err := c.client.LoadContainer(ctx, container.ID)
	if err != nil {
		return err
	}
	var delOpts []containerd.DeleteOpts
	delOpts = append(delOpts, containerd.WithSnapshotCleanup)

	if err := cont.Delete(ctx, delOpts...); err != nil {
		return err
	}

	log.Debugf("successfully deleted container %s", container.ID)

	return nil
}
