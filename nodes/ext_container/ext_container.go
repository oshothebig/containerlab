// Copyright 2020 Nokia
// Licensed under the BSD 3-Clause License.
// SPDX-License-Identifier: BSD-3-Clause

package ext_container

import (
	"context"
	"fmt"

	"github.com/pkg/errors"

	log "github.com/sirupsen/logrus"
	"github.com/srl-labs/containerlab/clab/exec"
	"github.com/srl-labs/containerlab/nodes"
	"github.com/srl-labs/containerlab/runtime"
	"github.com/srl-labs/containerlab/types"
)

var kindnames = []string{"ext-container"}

// Register registers the node in the global Node map.
func Register() {
	nodes.Register(kindnames, func() nodes.Node {
		return new(extcont)
	})
}

type extcont struct {
	nodes.DefaultNode
}

func (s *extcont) Init(cfg *types.NodeConfig, opts ...nodes.NodeOption) error {
	s.DefaultNode = *nodes.NewDefaultNode(s)
	s.Cfg = cfg
	for _, o := range opts {
		o(s)
	}
	return nil
}

func (e *extcont) GetContainers(ctx context.Context) ([]types.GenericContainer, error) {
	cnts, err := e.DefaultNode.GetContainers(ctx)
	if err != nil {
		return nil, err
	}

	if len(cnts) != 0 {
		return cnts, nil
	}

	// fallback: finding containers with short name as external containers are often unprefixed
	cnts, err = e.Runtime.ListContainers(ctx, []*types.GenericFilter{
		{
			FilterType: "name",
			Match:      fmt.Sprintf("^%s$", e.Cfg.ShortName),
		},
	})
	if err != nil {
		return nil, err
	}

	return cnts, nil
}

func (e *extcont) Deploy(ctx context.Context) error {
	// check for the external dependency to be running
	err := runtime.WaitForContainerRunning(ctx, e.Runtime, e.Cfg.ShortName, e.Cfg.ShortName)
	if err != nil {
		return err
	}

	// request nspath from runtime
	nspath, err := e.Runtime.GetNSPath(ctx, e.Cfg.ShortName)
	if err != nil {
		return errors.Wrap(err, "reading external container namespace path")
	}
	// set nspath in node config
	e.Cfg.NSPath = nspath
	// reflect ste nodes status as created
	e.Cfg.DeploymentStatus = "created"
	return nil
}

// Delete we will not mess with external containers on delete.
func (e *extcont) Delete(_ context.Context) error { return nil }

// GetImages don't matter for external containers.
func (e *extcont) GetImages(_ context.Context) map[string]string { return map[string]string{} }
func (e *extcont) PullImage(_ context.Context) error             { return nil }

func (e *extcont) UpdateConfigWithRuntimeInfo(ctx context.Context) error {
	cnts, err := e.GetContainers(ctx)
	if err != nil {
		return err
	}

	// TODO: rdodin: evaluate the necessity of this function, since runtime data may be updated by the runtime
	// when we do listing of containers and produce the GenericContainer
	// network settings of a first container only
	netSettings := cnts[0].NetworkSettings

	e.Cfg.MgmtIPv4Address = netSettings.IPv4addr
	e.Cfg.MgmtIPv4PrefixLength = netSettings.IPv4pLen
	e.Cfg.MgmtIPv6Address = netSettings.IPv6addr
	e.Cfg.MgmtIPv6PrefixLength = netSettings.IPv6pLen
	e.Cfg.MgmtIPv4Gateway = netSettings.IPv4Gw
	e.Cfg.MgmtIPv6Gateway = netSettings.IPv6Gw

	e.Cfg.ContainerID = cnts[0].ID

	return nil
}

// RunExecType is the final function that calls the runtime to execute a type.Exec on a container
// This is to be overriden if the nodes implementation differs.
func (e *extcont) RunExecType(ctx context.Context, execCmd *exec.ExecCmd) (exec.ExecResultHolder, error) {
	execResult, err := e.GetRuntime().Exec(ctx, e.Cfg.ShortName, execCmd)
	if err != nil {
		// On Ext-container we have to use the shortname, whilst default is to use longname
		log.Errorf("%s: failed to execute cmd: %q with error %v", e.Cfg.ShortName, execCmd.GetCmdString(), err)
		return nil, err
	}
	return execResult, nil
}

// RunExecType is the final function that calls the runtime to execute a type.Exec on a container
// This is to be overriden if the nodes implementation differs.
func (e *extcont) RunExecTypeWoWait(ctx context.Context, execCmd *exec.ExecCmd) error {
	err := e.GetRuntime().ExecNotWait(ctx, e.Cfg.ShortName, execCmd)
	if err != nil {
		// On Ext-container we have to use the shortname, whilst default is to use longname
		log.Errorf("%s: failed to execute cmd: %q with error %v", e.Cfg.ShortName, execCmd.GetCmdString(), err)
		return err
	}
	return nil
}
