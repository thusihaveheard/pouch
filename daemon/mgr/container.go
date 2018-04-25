package mgr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alibaba/pouch/apis/opts"
	"github.com/alibaba/pouch/apis/plugins"
	"github.com/alibaba/pouch/apis/types"
	"github.com/alibaba/pouch/ctrd"
	"github.com/alibaba/pouch/daemon/config"
	"github.com/alibaba/pouch/daemon/containerio"
	"github.com/alibaba/pouch/lxcfs"
	networktypes "github.com/alibaba/pouch/network/types"
	"github.com/alibaba/pouch/pkg/collect"
	"github.com/alibaba/pouch/pkg/errtypes"
	"github.com/alibaba/pouch/pkg/meta"
	"github.com/alibaba/pouch/pkg/randomid"
	"github.com/alibaba/pouch/pkg/reference"
	"github.com/alibaba/pouch/pkg/utils"
	"github.com/alibaba/pouch/storage/quota"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
	"github.com/docker/libnetwork"
	"github.com/go-openapi/strfmt"
	"github.com/imdario/mergo"
	"github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// ContainerMgr as an interface defines all operations against container.
type ContainerMgr interface {
	// Create a new container.
	Create(ctx context.Context, name string, config *types.ContainerCreateConfig) (*types.ContainerCreateResp, error)

	// Start a container.
	Start(ctx context.Context, id, detachKeys string) error

	// Stop a container.
	Stop(ctx context.Context, name string, timeout int64) error

	// Pause a container.
	Pause(ctx context.Context, name string) error

	// Unpause a container.
	Unpause(ctx context.Context, name string) error

	// Attach a container.
	Attach(ctx context.Context, name string, attach *AttachConfig) error

	// List returns the list of containers.
	List(ctx context.Context, filter ContainerFilter, option *ContainerListOption) ([]*ContainerMeta, error)

	// CreateExec creates exec process's environment.
	CreateExec(ctx context.Context, name string, config *types.ExecCreateConfig) (string, error)

	// StartExec executes a new process in container.
	StartExec(ctx context.Context, execid string, config *types.ExecStartConfig, attach *AttachConfig) error

	// InspectExec returns low-level information about exec command.
	InspectExec(ctx context.Context, execid string) (*types.ContainerExecInspect, error)

	// GetExecConfig returns execonfig of a exec process inside container.
	GetExecConfig(ctx context.Context, execid string) (*ContainerExecConfig, error)

	// Remove removes a container, it may be running or stopped and so on.
	Remove(ctx context.Context, name string, option *ContainerRemoveOption) error

	// Rename renames a container.
	Rename(ctx context.Context, oldName string, newName string) error

	// Get the detailed information of container.
	Get(ctx context.Context, name string) (*ContainerMeta, error)

	// Update updates the configurations of a container.
	Update(ctx context.Context, name string, config *types.UpdateConfig) error

	// Upgrade upgrades a container with new image and args.
	Upgrade(ctx context.Context, name string, config *types.ContainerUpgradeConfig) error

	// Top lists the processes running inside of the given container
	Top(ctx context.Context, name string, psArgs string) (*types.ContainerProcessList, error)

	// Resize resizes the size of container tty.
	Resize(ctx context.Context, name string, opts types.ResizeOptions) error

	// Restart restart a running container.
	Restart(ctx context.Context, name string, timeout int64) error

	// Connect is used to connect a container to a network.
	Connect(ctx context.Context, name string, networkIDOrName string, epConfig *types.EndpointSettings) error

	// DisconnectContainerFromNetwork disconnects the given container from
	// given network
	DisconnectContainerFromNetwork(ctx context.Context, containerName, networkName string, force bool) error
}

// ContainerManager is the default implement of interface ContainerMgr.
type ContainerManager struct {
	// Store stores containers in Backend store.
	// Element operated in store must has a type of *ContainerMeta.
	// By default, Store will use local filesystem with json format to store containers.
	Store *meta.Store

	// Client is used to interact with containerd.
	Client ctrd.APIClient

	// NameToID stores relations between container's name and ID.
	// It is used to get container ID via container name.
	NameToID *collect.SafeMap

	ImageMgr      ImageMgr
	VolumeMgr     VolumeMgr
	NetworkMgr    NetworkMgr
	IOs           *containerio.Cache
	ExecProcesses *collect.SafeMap

	Config *config.Config

	// Cache stores all containers in memory.
	// Element operated in cache must have a type of *Container.
	cache *collect.SafeMap

	// monitor is used to handle container's event, eg: exit, stop and so on.
	monitor *ContainerMonitor

	containerPlugin plugins.ContainerPlugin
}

// NewContainerManager creates a brand new container manager.
func NewContainerManager(ctx context.Context, store *meta.Store, cli ctrd.APIClient, imgMgr ImageMgr, volMgr VolumeMgr, netMgr NetworkMgr, cfg *config.Config, contPlugin plugins.ContainerPlugin) (*ContainerManager, error) {
	mgr := &ContainerManager{
		Store:           store,
		NameToID:        collect.NewSafeMap(),
		Client:          cli,
		ImageMgr:        imgMgr,
		VolumeMgr:       volMgr,
		NetworkMgr:      netMgr,
		IOs:             containerio.NewCache(),
		ExecProcesses:   collect.NewSafeMap(),
		cache:           collect.NewSafeMap(),
		Config:          cfg,
		monitor:         NewContainerMonitor(),
		containerPlugin: contPlugin,
	}

	mgr.Client.SetExitHooks(mgr.exitedAndRelease)
	mgr.Client.SetExecExitHooks(mgr.execExitedAndRelease)

	go mgr.execProcessGC()

	return mgr, mgr.Restore(ctx)
}

// Restore containers from meta store to memory and recover those container.
func (mgr *ContainerManager) Restore(ctx context.Context) error {
	fn := func(obj meta.Object) error {
		containerMeta, ok := obj.(*ContainerMeta)
		if !ok {
			// object has not type of ContainerMeta
			return nil
		}

		// map container's name to id.
		mgr.NameToID.Put(containerMeta.Name, containerMeta.ID)

		// put container into cache.
		mgr.cache.Put(containerMeta.ID, &Container{meta: containerMeta})

		if containerMeta.State.Status != types.StatusRunning {
			return nil
		}

		// recover the running container.
		io, err := mgr.openContainerIO(containerMeta.ID, containerMeta.Config.OpenStdin)
		if err != nil {
			logrus.Errorf("failed to recover container: %s,  %v", containerMeta.ID, err)
		}

		err = mgr.Client.RecoverContainer(ctx, containerMeta.ID, io)
		if err != nil && strings.Contains(err.Error(), "not found") {
			logrus.Infof("container %s not found, executes mark stopped and release resources", containerMeta.ID)
			if err := mgr.markStoppedAndRelease(&Container{meta: containerMeta}, nil); err != nil {
				logrus.Errorf("failed to mark container: %s stop status, err: %v", containerMeta.ID, err)
			}
		} else if err != nil {
			logrus.Errorf("failed to recover container: %s,  %v", containerMeta.ID, err)
			// release io
			io.Close()
			mgr.IOs.Remove(containerMeta.ID)
		}

		return nil
	}
	return mgr.Store.ForEach(fn)
}

// Remove removes a container, it may be running or stopped and so on.
func (mgr *ContainerManager) Remove(ctx context.Context, name string, option *ContainerRemoveOption) error {
	c, err := mgr.container(name)
	if err != nil {
		return err
	}
	c.Lock()
	defer c.Unlock()

	if !c.IsStopped() && !c.IsExited() && !c.IsCreated() && !option.Force {
		return fmt.Errorf("container: %s is not stopped, can't remove it without flag force", c.ID())
	}

	// if the container is running, force to stop it.
	if c.IsRunning() && option.Force {
		msg, err := mgr.Client.DestroyContainer(ctx, c.ID(), c.StopTimeout())
		if err != nil && !errtypes.IsNotfound(err) {
			return errors.Wrapf(err, "failed to destroy container: %s", c.ID())
		}
		if err := mgr.markStoppedAndRelease(c, msg); err != nil {
			return errors.Wrapf(err, "failed to mark container: %s stop status", c.ID())
		}
	}

	if err := mgr.detachVolumes(ctx, c.meta); err != nil {
		logrus.Errorf("failed to detach volume: %v", err)
	}

	// remove name
	mgr.NameToID.Remove(c.Name())

	// remove meta data
	if err := mgr.Store.Remove(c.meta.Key()); err != nil {
		logrus.Errorf("failed to remove container: %s meta store, %v", c.ID(), err)
	}

	// remove container cache
	mgr.cache.Remove(c.ID())

	// remove snapshot
	if err := mgr.Client.RemoveSnapshot(ctx, c.ID()); err != nil {
		logrus.Errorf("failed to remove container: %s snapshot, %v", c.ID(), err)
	}

	return nil
}

// CreateExec creates exec process's meta data.
func (mgr *ContainerManager) CreateExec(ctx context.Context, name string, config *types.ExecCreateConfig) (string, error) {
	c, err := mgr.container(name)
	if err != nil {
		return "", err
	}

	if !c.IsRunning() {
		return "", fmt.Errorf("container %s is not running", c.ID())
	}

	execid := randomid.Generate()
	execConfig := &ContainerExecConfig{
		ExecID:           execid,
		ExecCreateConfig: *config,
		ContainerID:      c.ID(),
	}

	mgr.ExecProcesses.Put(execid, execConfig)

	return execid, nil
}

// StartExec executes a new process in container.
func (mgr *ContainerManager) StartExec(ctx context.Context, execid string, config *types.ExecStartConfig, attach *AttachConfig) error {
	v, ok := mgr.ExecProcesses.Get(execid).Result()
	if !ok {
		return errors.Wrap(errtypes.ErrNotfound, "to be exec process: "+execid)
	}
	execConfig, ok := v.(*ContainerExecConfig)
	if !ok {
		return fmt.Errorf("invalid exec config type")
	}

	if attach != nil {
		attach.Stdin = execConfig.AttachStdin
	}

	io, err := mgr.openExecIO(execid, attach)
	if err != nil {
		return err
	}

	c, err := mgr.container(execConfig.ContainerID)
	if err != nil {
		return err
	}

	process := &specs.Process{
		Args:     execConfig.Cmd,
		Terminal: execConfig.Tty,
		Cwd:      "/",
		Env:      c.Config().Env,
	}

	if execConfig.User != "" {
		c.meta.Config.User = execConfig.User
	}

	if err = setupProcessUser(ctx, c.meta, &SpecWrapper{s: &specs.Spec{Process: process}}); err != nil {
		return err
	}

	execConfig.Running = true
	defer func() {
		if err != nil {
			execConfig.Running = false
			exitCode := 126
			execConfig.ExitCode = int64(exitCode)
		}
		mgr.ExecProcesses.Put(execid, execConfig)
	}()

	err = mgr.Client.ExecContainer(ctx, &ctrd.Process{
		ContainerID: execConfig.ContainerID,
		ExecID:      execid,
		IO:          io,
		P:           process,
	})

	return err
}

// InspectExec returns low-level information about exec command.
func (mgr *ContainerManager) InspectExec(ctx context.Context, execid string) (*types.ContainerExecInspect, error) {
	execConfig, err := mgr.GetExecConfig(ctx, execid)
	if err != nil {
		return nil, err
	}

	return &types.ContainerExecInspect{
		ID: execConfig.ExecID,
		// FIXME: try to use the correct running status of exec
		Running:     execConfig.Running,
		ExitCode:    execConfig.ExitCode,
		ContainerID: execConfig.ContainerID,
	}, nil
}

// GetExecConfig returns execonfig of a exec process inside container.
func (mgr *ContainerManager) GetExecConfig(ctx context.Context, execid string) (*ContainerExecConfig, error) {
	v, ok := mgr.ExecProcesses.Get(execid).Result()
	if !ok {
		return nil, errors.Wrap(errtypes.ErrNotfound, "exec process "+execid)
	}
	execConfig, ok := v.(*ContainerExecConfig)
	if !ok {
		return nil, fmt.Errorf("invalid exec config type")
	}
	return execConfig, nil
}

// Create checks passed in parameters and create a Container object whose status is set at Created.
func (mgr *ContainerManager) Create(ctx context.Context, name string, config *types.ContainerCreateConfig) (*types.ContainerCreateResp, error) {
	// TODO: check request validate.
	if config.HostConfig == nil {
		return nil, fmt.Errorf("HostConfig cannot be nil")
	}
	if config.NetworkingConfig == nil {
		return nil, fmt.Errorf("NetworkingConfig cannot be nil")
	}

	id, err := mgr.generateID()
	if err != nil {
		return nil, err
	}

	if name == "" {
		name = mgr.generateName(id)
	} else if mgr.NameToID.Get(name).Exist() {
		return nil, errors.Wrap(errtypes.ErrAlreadyExisted, "container name: "+name)
	}

	// check the image existed or not, and convert image id to image ref
	image, err := mgr.ImageMgr.GetImage(ctx, config.Image)
	if err != nil {
		return nil, err
	}

	// FIXME: image.Name does not exist,so convert Repotags or RepoDigests to ref
	// return the first item of list will not equal input image name.
	// issue: https://github.com/alibaba/pouch/issues/1001
	// if specify a tag image, we should use the specified name
	var refTagged string
	imageNamed, err := reference.ParseNamedReference(config.Image)
	if err != nil {
		return nil, err
	}
	if _, ok := imageNamed.(reference.Tagged); ok {
		refTagged = reference.WithDefaultTagIfMissing(imageNamed).String()
	}

	ref := ""
	if len(image.RepoTags) > 0 {
		if utils.StringInSlice(image.RepoTags, refTagged) {
			ref = refTagged
		} else {
			ref = image.RepoTags[0]
		}
	} else {
		ref = image.RepoDigests[0]
	}
	config.Image = ref

	// set container runtime
	if config.HostConfig.Runtime == "" {
		config.HostConfig.Runtime = mgr.Config.DefaultRuntime
	}

	// create a snapshot with image.
	if err := mgr.Client.CreateSnapshot(ctx, id, config.Image); err != nil {
		return nil, err
	}

	// Get snapshot UpperDir
	var upperDir string
	mounts, err := mgr.Client.GetMounts(ctx, id)
	if err != nil {
		return nil, err
	} else if len(mounts) != 1 {
		return nil, fmt.Errorf("failed to get snapshot %s mounts: not equals one", id)
	}
	for _, opt := range mounts[0].Options {
		if strings.HasPrefix(opt, "upperdir=") {
			upperDir = strings.TrimPrefix(opt, "upperdir=")
		}
	}

	// set lxcfs binds
	if config.HostConfig.EnableLxcfs && lxcfs.IsLxcfsEnabled {
		config.HostConfig.Binds = append(config.HostConfig.Binds, lxcfs.LxcfsParentDir+":/var/lib/lxc:shared")
		sourceDir := lxcfs.LxcfsHomeDir + "/proc/"
		destDir := "/proc/"
		for _, procFile := range lxcfs.LxcfsProcFiles {
			bind := fmt.Sprintf("%s%s:%s%s", sourceDir, procFile, destDir, procFile)
			config.HostConfig.Binds = append(config.HostConfig.Binds, bind)
		}
	}

	meta := &ContainerMeta{
		State: &types.ContainerState{
			Status:     types.StatusCreated,
			StartedAt:  time.Time{}.UTC().Format(utils.TimeLayout),
			FinishedAt: time.Time{}.UTC().Format(utils.TimeLayout),
		},
		ID:         id,
		Image:      image.ID,
		Name:       name,
		Config:     &config.ContainerConfig,
		Created:    time.Now().UTC().Format(utils.TimeLayout),
		HostConfig: config.HostConfig,
	}

	// parse volume config
	if err := mgr.parseBinds(ctx, meta); err != nil {
		return nil, errors.Wrap(err, "failed to parse volume argument")
	}

	// set mount point disk quota
	if err := mgr.setMountPointDiskQuota(ctx, meta); err != nil {
		return nil, errors.Wrap(err, "failed to set mount point disk quota")
	}

	// set container basefs
	mgr.setBaseFS(ctx, meta, id)

	// set network settings
	networkMode := config.HostConfig.NetworkMode
	if networkMode == "" {
		config.HostConfig.NetworkMode = "bridge"
		meta.Config.NetworkDisabled = true
	}
	meta.NetworkSettings = new(types.NetworkSettings)
	if len(config.NetworkingConfig.EndpointsConfig) > 0 {
		meta.NetworkSettings.Networks = config.NetworkingConfig.EndpointsConfig
	}
	if meta.NetworkSettings.Networks == nil && networkMode != "" && !IsContainer(networkMode) {
		meta.NetworkSettings.Networks = make(map[string]*types.EndpointSettings)
		meta.NetworkSettings.Networks[config.HostConfig.NetworkMode] = new(types.EndpointSettings)
	}

	if err := parseSecurityOpts(meta, config.HostConfig.SecurityOpt); err != nil {
		return nil, err
	}

	// merge image's config into container's meta
	if err := meta.merge(func() (v1.ImageConfig, error) {
		ociimage, err := mgr.Client.GetOciImage(ctx, config.Image)
		if err != nil {
			return ociimage.Config, err
		}
		return ociimage.Config, nil
	}); err != nil {
		return nil, err
	}

	// set snapshotter for container
	// TODO(ziren): now we only support overlayfs
	meta.Snapshotter = &types.SnapshotterData{
		Name: "overlayfs",
		Data: map[string]string{},
	}

	if upperDir != "" {
		meta.Snapshotter.Data["UpperDir"] = upperDir
	}

	container := &Container{
		meta: meta,
	}

	container.Lock()
	defer container.Unlock()

	// store disk
	container.Write(mgr.Store)

	// add to collection
	mgr.NameToID.Put(name, id)
	mgr.cache.Put(id, container)

	return &types.ContainerCreateResp{
		ID:   id,
		Name: name,
	}, nil
}

// Start a pre created Container.
func (mgr *ContainerManager) Start(ctx context.Context, id, detachKeys string) (err error) {
	if id == "" {
		return errors.Wrap(errtypes.ErrInvalidParam, "either container name or id is required")
	}

	c, err := mgr.container(id)
	if err != nil {
		return err
	}

	c.Lock()
	defer c.Unlock()

	return mgr.start(ctx, c, detachKeys)
}

func (mgr *ContainerManager) start(ctx context.Context, c *Container, detachKeys string) error {
	if c.meta.Config == nil || c.meta.State == nil {
		return errors.Wrap(errtypes.ErrNotfound, "container "+c.ID())
	}
	c.DetachKeys = detachKeys

	// initialise container network mode
	networkMode := c.meta.HostConfig.NetworkMode
	if IsContainer(networkMode) {
		origContainer, err := mgr.Get(ctx, strings.SplitN(networkMode, ":", 2)[1])
		if err != nil {
			return err
		}

		c.meta.HostnamePath = origContainer.HostnamePath
		c.meta.HostsPath = origContainer.HostsPath
		c.meta.ResolvConfPath = origContainer.ResolvConfPath
		c.meta.Config.Hostname = origContainer.Config.Hostname
		c.meta.Config.Domainname = origContainer.Config.Domainname
	}

	// initialise host network mode
	if IsHost(networkMode) {
		hostname, err := os.Hostname()
		if err != nil {
			return err
		}
		c.meta.Config.Hostname = strfmt.Hostname(hostname)
	}

	// initialise network endpoint
	if c.meta.NetworkSettings != nil {
		for name, endpointSetting := range c.meta.NetworkSettings.Networks {
			endpoint := mgr.buildContainerEndpoint(c.meta)
			endpoint.Name = name
			endpoint.EndpointConfig = endpointSetting
			if _, err := mgr.NetworkMgr.EndpointCreate(ctx, endpoint); err != nil {
				logrus.Errorf("failed to create endpoint: %v", err)
				return err
			}
		}
	}

	return mgr.createContainerdContainer(ctx, c)
}

func (mgr *ContainerManager) createContainerdContainer(ctx context.Context, c *Container) error {
	// new a default spec.
	s, err := ctrd.NewDefaultSpec(ctx, c.ID())
	if err != nil {
		return errors.Wrapf(err, "failed to generate spec: %s", c.ID())
	}

	var cgroupsParent string
	if c.meta.HostConfig.CgroupParent != "" {
		cgroupsParent = c.meta.HostConfig.CgroupParent
	} else if mgr.Config.CgroupParent != "" {
		cgroupsParent = mgr.Config.CgroupParent
	}

	// cgroupsPath must be absolute path
	// call filepath.Clean is to avoid bad
	// path just like../../../.../../BadPath
	if cgroupsParent != "" {
		if !filepath.IsAbs(cgroupsParent) {
			cgroupsParent = filepath.Clean("/" + cgroupsParent)
		}

		s.Linux.CgroupsPath = filepath.Join(cgroupsParent, c.ID())
	}

	var prioArr []int
	var argsArr [][]string
	if mgr.containerPlugin != nil {
		prioArr, argsArr, err = mgr.containerPlugin.PreStart(c)
		if err != nil {
			return errors.Wrapf(err, "get pre-start hook error from container plugin")
		}
	}

	sw := &SpecWrapper{
		s:       s,
		ctrMgr:  mgr,
		volMgr:  mgr.VolumeMgr,
		netMgr:  mgr.NetworkMgr,
		prioArr: prioArr,
		argsArr: argsArr,
	}

	for _, setup := range SetupFuncs() {
		if err = setup(ctx, c.meta, sw); err != nil {
			return err
		}
	}

	// open container's stdio.
	io, err := mgr.openContainerIO(c.ID(), c.meta.Config.OpenStdin)
	if err != nil {
		return errors.Wrap(err, "failed to open io")
	}
	if io.Stdin != nil && io.Stdin.OpenStdin() {
		s.Process.Terminal = true
	}

	if err := mgr.Client.CreateContainer(ctx, &ctrd.Container{
		ID:      c.ID(),
		Image:   c.Image(),
		Runtime: c.meta.HostConfig.Runtime,
		Spec:    s,
		IO:      io,
	}); err != nil {
		logrus.Errorf("failed to create new containerd container: %s", err.Error())

		// TODO(ziren): markStoppedAndRelease may failed
		// we should clean resources of container when start failed
		_ = mgr.markStoppedAndRelease(c, nil)
		return err
	}

	// Create containerd container success.
	c.meta.State.Status = types.StatusRunning
	c.meta.State.StartedAt = time.Now().UTC().Format(utils.TimeLayout)
	pid, err := mgr.Client.ContainerPID(ctx, c.ID())
	if err != nil {
		return errors.Wrapf(err, "failed to get PID of container %s", c.ID())
	}
	c.meta.State.Pid = int64(pid)
	c.meta.State.ExitCode = 0

	// set Snapshot MergedDir
	c.meta.Snapshotter.Data["MergedDir"] = c.meta.BaseFS

	return c.Write(mgr.Store)
}

// Stop stops a running container.
func (mgr *ContainerManager) Stop(ctx context.Context, name string, timeout int64) error {
	var (
		err error
		c   *Container
	)

	if c, err = mgr.container(name); err != nil {
		return err
	}

	c.Lock()
	defer c.Unlock()

	if !c.IsRunning() {
		return fmt.Errorf("container's status is not running: %s", c.meta.State.Status)
	}

	if timeout == 0 {
		timeout = c.StopTimeout()
	}

	return mgr.stop(ctx, c, timeout)
}

func (mgr *ContainerManager) stop(ctx context.Context, c *Container, timeout int64) error {
	msg, err := mgr.Client.DestroyContainer(ctx, c.ID(), timeout)
	if err != nil {
		return errors.Wrapf(err, "failed to destroy container: %s", c.ID())
	}

	return mgr.markStoppedAndRelease(c, msg)
}

// Pause pauses a running container.
func (mgr *ContainerManager) Pause(ctx context.Context, name string) error {
	var (
		err error
		c   *Container
	)

	if c, err = mgr.container(name); err != nil {
		return err
	}

	c.Lock()
	defer c.Unlock()

	if c.meta.Config == nil || c.meta.State == nil {
		return errors.Wrap(errtypes.ErrNotfound, "container "+c.ID())
	}

	if !c.IsRunning() {
		return fmt.Errorf("container's status is not running: %s", c.meta.State.Status)
	}

	if err := mgr.Client.PauseContainer(ctx, c.ID()); err != nil {
		return errors.Wrapf(err, "failed to pause container: %s", c.ID())
	}

	c.meta.State.Status = types.StatusPaused
	c.Write(mgr.Store)
	return nil
}

// Unpause unpauses a paused container.
func (mgr *ContainerManager) Unpause(ctx context.Context, name string) error {
	var (
		err error
		c   *Container
	)

	if c, err = mgr.container(name); err != nil {
		return err
	}

	c.Lock()
	defer c.Unlock()

	if c.meta.Config == nil || c.meta.State == nil {
		return errors.Wrap(errtypes.ErrNotfound, "container "+c.ID())
	}

	if !c.IsPaused() {
		return fmt.Errorf("container's status is not paused: %v", c.meta.State.Status)
	}

	if err := mgr.Client.UnpauseContainer(ctx, c.ID()); err != nil {
		return errors.Wrapf(err, "failed to unpause container: %s", c.ID())
	}

	c.meta.State.Status = types.StatusRunning
	c.Write(mgr.Store)
	return nil
}

// Attach attachs a container's io.
func (mgr *ContainerManager) Attach(ctx context.Context, name string, attach *AttachConfig) error {
	c, err := mgr.container(name)
	if err != nil {
		return err
	}

	_, err = mgr.openAttachIO(c.ID(), attach)
	if err != nil {
		return err
	}

	return nil
}

// List returns the container's list.
func (mgr *ContainerManager) List(ctx context.Context, filter ContainerFilter, option *ContainerListOption) ([]*ContainerMeta, error) {
	metas := []*ContainerMeta{}

	list, err := mgr.Store.List()
	if err != nil {
		return nil, err
	}

	for _, obj := range list {
		m, ok := obj.(*ContainerMeta)
		if !ok {
			return nil, fmt.Errorf("failed to get container list, invalid meta type")
		}
		if filter != nil && filter(m) {
			if option.All {
				metas = append(metas, m)
			} else if m.State.Status == types.StatusRunning || m.State.Status == types.StatusPaused {
				metas = append(metas, m)
			}
		}
	}

	return metas, nil
}

// Get the detailed information of container.
func (mgr *ContainerManager) Get(ctx context.Context, name string) (*ContainerMeta, error) {
	c, err := mgr.container(name)
	if err != nil {
		return nil, err
	}

	c.Lock()
	defer c.Unlock()

	return c.meta, nil
}

// Rename renames a container
func (mgr *ContainerManager) Rename(ctx context.Context, oldName, newName string) error {
	var (
		c   *Container
		err error
	)

	if mgr.NameToID.Get(newName).Exist() {
		return errors.Wrap(errtypes.ErrAlreadyExisted, "container name: "+newName)
	}

	if c, err = mgr.container(oldName); err != nil {
		return errors.Wrap(err, "failed to rename container")
	}
	c.Lock()
	defer c.Unlock()

	mgr.NameToID.Remove(c.Name())
	mgr.NameToID.Put(newName, c.ID())

	c.meta.Name = newName
	c.Write(mgr.Store)

	return nil
}

// Update updates the configurations of a container.
func (mgr *ContainerManager) Update(ctx context.Context, name string, config *types.UpdateConfig) error {
	c, err := mgr.container(name)
	if err != nil {
		return err
	}

	c.Lock()
	defer c.Unlock()

	// update ContainerConfig of a container.
	if !c.IsStopped() && config.Image != "" || len(config.Env) > 0 {
		return fmt.Errorf("Only can update the container's image or Env when it is stopped")
	}

	if config.Image != "" {
		image, err := mgr.ImageMgr.GetImage(ctx, config.Image)
		if err != nil {
			return err
		}
		// TODO Image param is duplicate in ContainerMeta
		// FIXME: image.Name does not exist,so convert Repotags or RepoDigests to ref
		// return the first item of list will not equal input image name.
		// issue: https://github.com/alibaba/pouch/issues/1001
		// if specify a tag image, we should use the specified name
		var refTagged string
		imageNamed, err := reference.ParseNamedReference(config.Image)
		if err != nil {
			return err
		}
		if _, ok := imageNamed.(reference.Tagged); ok {
			refTagged = reference.WithDefaultTagIfMissing(imageNamed).String()
		}

		ref := ""
		if len(image.RepoTags) > 0 {
			if utils.StringInSlice(image.RepoTags, refTagged) {
				ref = refTagged
			} else {
				ref = image.RepoTags[0]
			}
		} else {
			ref = image.RepoDigests[0]
		}
		c.meta.Config.Image = ref
		c.meta.Image = ref
	}

	if len(config.Env) != 0 {
		for k, v := range config.Env {
			c.meta.Config.Env[k] = v
		}
	}

	if len(config.Labels) != 0 {
		for k, v := range config.Labels {
			c.meta.Config.Labels[k] = v
		}
	}

	// update resources of container.
	resources := config.Resources
	cResources := &c.meta.HostConfig.Resources
	if resources.BlkioWeight != 0 {
		cResources.BlkioWeight = resources.BlkioWeight
	}
	if resources.CPUShares != 0 {
		cResources.CPUShares = resources.CPUShares
	}
	if resources.CpusetCpus != "" {
		cResources.CpusetCpus = resources.CpusetCpus
	}
	if resources.CpusetMems != "" {
		cResources.CpusetMems = resources.CpusetMems
	}
	if resources.Memory != 0 {
		cResources.Memory = resources.Memory
	}
	if resources.MemorySwap != 0 {
		cResources.MemorySwap = resources.MemorySwap
	}

	// update HostConfig of a container.
	// TODO update restartpolicy when container is running.
	if config.RestartPolicy.Name != "" {
		c.meta.HostConfig.RestartPolicy = config.RestartPolicy
	}

	// If container is not running, update container metadata struct is enough,
	// resources will be updated when the container is started again,
	// If container is running, we need to update configs to the real world.
	var updateErr error
	if c.IsRunning() {
		updateErr = mgr.Client.UpdateResources(ctx, c.ID(), c.meta.HostConfig.Resources)
	}

	// store disk.
	if updateErr == nil {
		c.Write(mgr.Store)
	}

	return updateErr
}

// Upgrade upgrades a container with new image and args.
func (mgr *ContainerManager) Upgrade(ctx context.Context, name string, config *types.ContainerUpgradeConfig) error {
	c, err := mgr.container(name)
	if err != nil {
		return err
	}

	c.Lock()
	defer c.Unlock()

	// check the image existed or not, and convert image id to image ref
	image, err := mgr.ImageMgr.GetImage(ctx, config.Image)
	if err != nil {
		return errors.Wrap(err, "failed to get image")
	}

	// FIXME: image.Name does not exist,so convert Repotags or RepoDigests to ref
	ref := ""
	if len(image.RepoTags) > 0 {
		ref = image.RepoTags[0]
	} else {
		ref = image.RepoDigests[0]
	}
	config.Image = ref

	// Nothing changed, no need upgrade.
	if config.Image == c.Image() {
		return fmt.Errorf("failed to upgrade container: image not changed")
	}

	var (
		needRollback        = false
		backupContainerMeta = *c.meta
	)

	defer func() {
		if needRollback {
			c.meta = &backupContainerMeta
			if err := mgr.Client.CreateSnapshot(ctx, c.ID(), c.meta.Image); err != nil {
				logrus.Errorf("failed to create snapshot when rollback upgrade action: %v", err)
				return
			}
			// FIXME: create new containerd container may failed
			_ = mgr.createContainerdContainer(ctx, c)
		}
	}()

	// FIXME(ziren): mergo.Merge() use AppendSlice to merge slice.
	// that is to say, t1 = ["a", "b"], t2 = ["a", "c"], the merge
	// result will be ["a", "b", "a", "c"]
	// This may occur errors, just take notes to record this.
	if err := mergo.MergeWithOverwrite(c.meta.Config, config.ContainerConfig); err != nil {
		return errors.Wrapf(err, "failed to merge ContainerConfig")
	}
	if err := mergo.MergeWithOverwrite(c.meta.HostConfig, config.HostConfig); err != nil {
		return errors.Wrapf(err, "failed to merge HostConfig")
	}
	c.meta.Image = config.Image

	// If container is running,  we need change
	// configuration and recreate it. Else we just store new meta
	// into disk, next time when starts container, the new configurations
	// will take effect.
	if c.IsRunning() {
		// Inherit volume configurations from old container,
		// New volume configurations may cover the old one.
		// c.meta.VolumesFrom = []string{c.ID()}

		// FIXME(ziren): here will forcely stop container afer 3s.
		// If DestroyContainer failed, we think the old container
		// not changed, so just return error, no need recover it.
		_, err := mgr.Client.DestroyContainer(ctx, c.ID(), 3)
		if err != nil {
			return errors.Wrapf(err, "failed to destroy container")
		}

		// remove snapshot of old container
		if err := mgr.Client.RemoveSnapshot(ctx, c.ID()); err != nil {
			return errors.Wrap(err, "failed to remove snapshot")
		}

		// wait util old snapshot to be deleted
		wait := make(chan struct{})
		go func() {
			for {
				// FIXME(ziren) Ensure the removed snapshot be removed
				// by garbage collection.
				time.Sleep(100 * time.Millisecond)

				_, err := mgr.Client.GetSnapshot(ctx, c.ID())
				if err != nil && errdefs.IsNotFound(err) {
					close(wait)
					return
				}
			}
		}()

		select {
		case <-wait:
			// TODO delete snapshot succeeded
		case <-time.After(30 * time.Second):
			needRollback = true
			return fmt.Errorf("failed to deleted old snapshot: wait old snapshot %s to be deleted timeout(30s)", c.ID())
		}

		// create a snapshot with image for new container.
		if err := mgr.Client.CreateSnapshot(ctx, c.ID(), config.Image); err != nil {
			needRollback = true
			return errors.Wrap(err, "failed to create snapshot")
		}

		if err := mgr.createContainerdContainer(ctx, c); err != nil {
			needRollback = true
			return errors.Wrap(err, "failed to create new container")
		}

		// Upgrade succeeded, refresh the cache
		mgr.cache.Put(c.ID(), c)
	}

	// Works fine, store new container info to disk.
	c.Write(mgr.Store)

	return nil
}

// Top lists the processes running inside of the given container
func (mgr *ContainerManager) Top(ctx context.Context, name string, psArgs string) (*types.ContainerProcessList, error) {
	if psArgs == "" {
		psArgs = "-ef"
	}
	c, err := mgr.container(name)
	if err != nil {
		return nil, err
	}

	c.Lock()
	defer c.Unlock()

	if !c.IsRunning() {
		return nil, fmt.Errorf("container is not running, can not execute top command")
	}

	pids, err := mgr.Client.ContainerPIDs(ctx, c.ID())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get pids of container")
	}

	output, err := exec.Command("ps", strings.Split(psArgs, " ")...).Output()
	if err != nil {
		return nil, errors.Wrapf(err, "error running ps")
	}

	procList, err := parsePSOutput(output, pids)
	if err != nil {
		return nil, errors.Wrapf(err, "parsePSOutput failed")
	}

	return procList, nil
}

// Resize resizes the size of a container tty.
func (mgr *ContainerManager) Resize(ctx context.Context, name string, opts types.ResizeOptions) error {
	c, err := mgr.container(name)
	if err != nil {
		return err
	}

	c.Lock()
	defer c.Unlock()

	if !c.IsRunning() && !c.IsPaused() {
		return fmt.Errorf("failed to resize container %s: container is not running", c.ID())
	}

	return mgr.Client.ResizeContainer(ctx, c.ID(), opts)
}

// Restart restarts a running container.
func (mgr *ContainerManager) Restart(ctx context.Context, name string, timeout int64) error {
	c, err := mgr.container(name)
	if err != nil {
		return err
	}

	c.Lock()
	defer c.Unlock()

	if !c.IsRunning() {
		return fmt.Errorf("cannot restart a non running container")
	}

	if timeout == 0 {
		timeout = c.StopTimeout()
	}

	// stop container
	if err := mgr.stop(ctx, c, timeout); err != nil {
		return errors.Wrapf(err, "failed to stop container")
	}
	logrus.Debug("Restart: container " + c.ID() + "  stopped succeeded")

	// start container
	return mgr.start(ctx, c, "")
}

// Connect is used to connect a container to a network.
func (mgr *ContainerManager) Connect(ctx context.Context, name string, networkIDOrName string, epConfig *types.EndpointSettings) error {
	c, err := mgr.container(name)
	if err != nil {
		return errors.Wrapf(err, "failed to get container: %s", name)
	} else if c == nil {
		return fmt.Errorf("container: %s is not exist", name)
	}

	n, err := mgr.NetworkMgr.Get(context.Background(), networkIDOrName)
	if err != nil {
		return errors.Wrapf(err, "failed to get network: %s", networkIDOrName)
	} else if n == nil {
		return fmt.Errorf("network: %s is not exist", networkIDOrName)
	}

	if epConfig == nil {
		epConfig = &types.EndpointSettings{}
	}

	c.Lock()
	defer c.Unlock()

	if c.meta.State.Status != types.StatusRunning {
		if c.meta.State.Status == types.StatusDead {
			return fmt.Errorf("Container %s is marked for removal and cannot be connected or disconnected to the network", c.meta.ID)
		}

		if err := mgr.updateNetworkConfig(c.meta, n.Name, epConfig); err != nil {
			return err
		}
	} else if err := mgr.connectToNetwork(ctx, c.meta, networkIDOrName, epConfig); err != nil {
		return err
	}

	return c.Write(mgr.Store)
}

// DisconnectContainerFromNetwork disconnects the given container from
// given network
func (mgr *ContainerManager) DisconnectContainerFromNetwork(ctx context.Context, containerName, networkName string, force bool) error {
	c, err := mgr.container(containerName)
	if err != nil {
		// TODO(ziren): if force is true, force delete endpoint
		return err
	}

	// Get network
	network, err := mgr.NetworkMgr.Get(ctx, networkName)
	if err != nil {
		return fmt.Errorf("failed to get network %s when disconnecting container %s: %v", networkName, c.Name(), err)
	}

	// container cannot be disconnected from host network
	networkMode := c.meta.HostConfig.NetworkMode
	if IsHost(networkMode) && IsHost(network.Mode) {
		return fmt.Errorf("container cannot be disconnected from host network or connected to hostnetwork ")
	}

	networkSettings := c.meta.NetworkSettings
	if networkSettings == nil {
		return nil
	}

	epConfig, ok := networkSettings.Networks[network.Name]
	if !ok {
		// container not attached to the given network
		return fmt.Errorf("failed to disconnect container from network: container %s not attach to %s", c.Name(), networkName)
	}

	endpoint := mgr.buildContainerEndpoint(c.meta)
	endpoint.Name = network.Name
	endpoint.EndpointConfig = epConfig
	if err := mgr.NetworkMgr.EndpointRemove(ctx, endpoint); err != nil {
		logrus.Errorf("failed to remove endpoint: %v", err)
		return err
	}

	// disconnect an endpoint success, delete endpoint info from container json
	delete(networkSettings.Networks, network.Name)

	// if container has no network attached any more, set NetworkDisabled to true
	// so that not setup Network Namespace when restart the container
	if len(networkSettings.Networks) == 0 {
		c.meta.Config.NetworkDisabled = true
	}

	// container meta changed, refresh the cache
	mgr.cache.Put(c.ID(), c)

	// update container meta json
	if err := c.Write(mgr.Store); err != nil {
		logrus.Errorf("failed to update meta: %v", err)
	}

	return nil
}

func (mgr *ContainerManager) openContainerIO(id string, stdin bool) (*containerio.IO, error) {
	if io := mgr.IOs.Get(id); io != nil {
		return io, nil
	}

	root := mgr.Store.Path(id)
	options := []func(*containerio.Option){
		containerio.WithID(id),
		containerio.WithRootDir(root),
		containerio.WithRawFile(),
		containerio.WithStdin(stdin),
	}

	io := containerio.NewIO(containerio.NewOption(options...))

	mgr.IOs.Put(id, io)

	return io, nil
}

func (mgr *ContainerManager) updateNetworkConfig(container *ContainerMeta, networkIDOrName string, endpointConfig *types.EndpointSettings) error {
	if IsContainer(container.HostConfig.NetworkMode) {
		return fmt.Errorf("container sharing network namespace with another container or host cannot be connected to any other network")
	}

	// TODO check bridge-mode conflict

	if IsUserDefined(container.HostConfig.NetworkMode) {
		if hasUserDefinedIPAddress(endpointConfig) {
			return fmt.Errorf("user specified IP address is supported on user defined networks only")
		}
		if endpointConfig != nil && len(endpointConfig.Aliases) > 0 {
			return fmt.Errorf("network-scoped alias is supported only for containers in user defined networks")
		}
	} else {
		addShortID := true
		shortID := utils.TruncateID(container.ID)
		for _, alias := range endpointConfig.Aliases {
			if alias == shortID {
				addShortID = false
				break
			}
		}
		if addShortID {
			endpointConfig.Aliases = append(endpointConfig.Aliases, shortID)
		}
	}

	network, err := mgr.NetworkMgr.Get(context.Background(), networkIDOrName)
	if err != nil {
		return err
	}

	if err := validateNetworkingConfig(network.Network, endpointConfig); err != nil {
		return err
	}

	container.NetworkSettings.Networks[network.Name] = endpointConfig

	return nil
}

func (mgr *ContainerManager) connectToNetwork(ctx context.Context, container *ContainerMeta, networkIDOrName string, epConfig *types.EndpointSettings) (err error) {
	if IsContainer(container.HostConfig.NetworkMode) {
		return fmt.Errorf("container sharing network namespace with another container or host cannot be connected to any other network")
	}

	// TODO check bridge mode conflict

	network, err := mgr.NetworkMgr.Get(context.Background(), networkIDOrName)
	if err != nil {
		return errors.Wrap(err, "failed to get network")
	}

	endpoint := mgr.buildContainerEndpoint(container)
	endpoint.Name = network.Name
	endpoint.EndpointConfig = epConfig
	if _, err := mgr.NetworkMgr.EndpointCreate(ctx, endpoint); err != nil {
		logrus.Errorf("failed to create endpoint: %v", err)
		return err
	}

	return mgr.updateNetworkConfig(container, networkIDOrName, endpoint.EndpointConfig)
}

func (mgr *ContainerManager) updateNetworkSettings(container *ContainerMeta, n libnetwork.Network) error {
	if container.NetworkSettings == nil {
		container.NetworkSettings = &types.NetworkSettings{Networks: make(map[string]*types.EndpointSettings)}
	}

	if !IsHost(container.HostConfig.NetworkMode) && IsHost(n.Type()) {
		return fmt.Errorf("container cannot be connected to host network")
	}

	for s := range container.NetworkSettings.Networks {
		sn, err := mgr.NetworkMgr.Get(context.Background(), s)
		if err != nil {
			continue
		}

		if sn.Name == n.Name() {
			// Avoid duplicate config
			return nil
		}
		if !IsPrivate(sn.Type) || !IsPrivate(n.Type()) {
			return fmt.Errorf("container sharing network namespace with another container or host cannot be connected to any other network")
		}
		if IsNone(sn.Name) || IsNone(n.Name()) {
			return fmt.Errorf("container cannot be connected to multiple networks with one of the networks in none mode")
		}
	}

	if _, ok := container.NetworkSettings.Networks[n.Name()]; !ok {
		container.NetworkSettings.Networks[n.Name()] = new(types.EndpointSettings)
	}

	return nil
}

func (mgr *ContainerManager) openExecIO(id string, attach *AttachConfig) (*containerio.IO, error) {
	if io := mgr.IOs.Get(id); io != nil {
		return io, nil
	}

	options := []func(*containerio.Option){
		containerio.WithID(id),
		containerio.WithStdin(attach.Stdin),
	}

	if attach != nil {
		options = append(options, attachConfigToOptions(attach)...)
	} else {
		options = append(options, containerio.WithDiscard())
	}

	io := containerio.NewIO(containerio.NewOption(options...))

	mgr.IOs.Put(id, io)

	return io, nil
}

func (mgr *ContainerManager) openAttachIO(id string, attach *AttachConfig) (*containerio.IO, error) {
	options := []func(*containerio.Option){
		containerio.WithID(id),
		containerio.WithStdin(attach.Stdin),
	}
	if attach != nil {
		options = append(options, attachConfigToOptions(attach)...)
	} else {
		options = append(options, containerio.WithDiscard())
	}

	io := mgr.IOs.Get(id)
	if io != nil {
		io.AddBackend(containerio.NewOption(options...))
	} else {
		io = containerio.NewIO(containerio.NewOption(options...))
	}

	mgr.IOs.Put(id, io)

	return io, nil
}

func attachConfigToOptions(attach *AttachConfig) []func(*containerio.Option) {
	options := []func(*containerio.Option){}
	if attach.Hijack != nil {
		// Attaching using http.
		options = append(options, containerio.WithHijack(attach.Hijack, attach.Upgrade))
		if attach.Stdin {
			options = append(options, containerio.WithStdinHijack())
		}
	} else if attach.MemBuffer != nil {
		// Attaching using memory buffer.
		options = append(options, containerio.WithMemBuffer(attach.MemBuffer))
	} else if attach.Streams != nil {
		// Attaching using streams.
		options = append(options, containerio.WithStreams(attach.Streams))
		if attach.Stdin {
			options = append(options, containerio.WithStdinStream())
		}
	}

	if attach.CriLogFile != nil {
		options = append(options, containerio.WithCriLogFile(attach.CriLogFile))
	}

	return options
}

func (mgr *ContainerManager) markStoppedAndRelease(c *Container, m *ctrd.Message) error {
	c.meta.State.Pid = -1
	c.meta.State.FinishedAt = time.Now().UTC().Format(utils.TimeLayout)
	c.meta.State.Status = types.StatusStopped

	if m != nil {
		c.meta.State.ExitCode = int64(m.ExitCode())
		if err := m.RawError(); err != nil {
			c.meta.State.Error = err.Error()
		}
	}

	// release resource
	if io := mgr.IOs.Get(c.ID()); io != nil {
		io.Close()
		mgr.IOs.Remove(c.ID())
	}

	// release network
	if c.meta.NetworkSettings != nil {
		for name, epConfig := range c.meta.NetworkSettings.Networks {
			endpoint := mgr.buildContainerEndpoint(c.meta)
			endpoint.Name = name
			endpoint.EndpointConfig = epConfig
			if err := mgr.NetworkMgr.EndpointRemove(context.Background(), endpoint); err != nil {
				logrus.Errorf("failed to remove endpoint: %v", err)
				return err
			}
		}
	}

	// unset Snapshot MergedDir. Stop a container will
	// delete the containerd container, the merged dir
	// will also be deleted, so we should unset the
	// container's MergedDir.
	if c.meta.Snapshotter != nil && c.meta.Snapshotter.Data != nil {
		c.meta.Snapshotter.Data["MergedDir"] = ""
	}

	// update meta
	if err := c.Write(mgr.Store); err != nil {
		logrus.Errorf("failed to update meta: %v", err)
	}
	return nil
}

// exitedAndRelease be register into ctrd as a callback function, when the running container suddenly
// exited, "ctrd" will call it to set the container's state and release resouce and so on.
func (mgr *ContainerManager) exitedAndRelease(id string, m *ctrd.Message) error {
	// update container info
	c, err := mgr.container(id)
	if err != nil {
		return err
	}

	c.Lock()
	defer c.Unlock()

	if err := mgr.markStoppedAndRelease(c, m); err != nil {
		return err
	}

	c.meta.State.Status = types.StatusExited
	if err := c.Write(mgr.Store); err != nil {
		logrus.Errorf("failed to update meta: %v", err)
	}

	// send exit event to monitor
	mgr.monitor.PostEvent(ContainerExitEvent(c).WithHandle(func(container *Container) error {
		// check status and restart policy
		container.Lock()

		if !container.IsExited() {
			container.Unlock()
			return nil
		}
		policy := (*ContainerRestartPolicy)(container.HostConfig().RestartPolicy)
		if policy == nil || policy.IsNone() {
			container.Unlock()
			return nil
		}

		container.Unlock()

		return mgr.Start(context.TODO(), container.ID(), container.DetachKeys)
	}))

	return nil
}

// execExitedAndRelease be register into ctrd as a callback function, when the exec process in a container
// exited, "ctrd" will call it to release resource and so on.
func (mgr *ContainerManager) execExitedAndRelease(id string, m *ctrd.Message) error {
	if io := mgr.IOs.Get(id); io != nil {
		if err := m.RawError(); err != nil {
			fmt.Fprintf(io.Stdout, "%v\n", err)
		}

		// close io
		io.Close()
		mgr.IOs.Remove(id)
	}

	v, ok := mgr.ExecProcesses.Get(id).Result()
	if !ok {
		return errors.Wrap(errtypes.ErrNotfound, "to be exec process: "+id)
	}
	execConfig, ok := v.(*ContainerExecConfig)
	if !ok {
		return fmt.Errorf("invalid exec config type")
	}
	execConfig.ExitCode = int64(m.ExitCode())
	execConfig.Running = false
	execConfig.Error = m.RawError()

	// TODO: GC invalid mgr.ExecProcess.
	return nil
}

func (mgr *ContainerManager) bindVolume(ctx context.Context, name string, meta *ContainerMeta) (string, string, error) {
	id := meta.ID

	ref := ""
	driver := "local"
	v, err := mgr.VolumeMgr.Get(ctx, name)
	if err != nil || v == nil {
		opts := map[string]string{
			"backend": "local",
		}
		if err := mgr.VolumeMgr.Create(ctx, name, meta.HostConfig.VolumeDriver, opts, nil); err != nil {
			logrus.Errorf("failed to create volume: %s, err: %v", name, err)
			return "", "", errors.Wrap(err, "failed to create volume")
		}
	} else {
		ref = v.Option("ref")
		driver = v.Driver()
	}

	option := map[string]string{}
	if ref == "" {
		option["ref"] = id
	} else {
		option["ref"] = ref + "," + id
	}
	if _, err := mgr.VolumeMgr.Attach(ctx, name, option); err != nil {
		logrus.Errorf("failed to attach volume: %s, err: %v", name, err)
		return "", "", errors.Wrap(err, "failed to attach volume")
	}

	mountPath, err := mgr.VolumeMgr.Path(ctx, name)
	if err != nil {
		logrus.Errorf("failed to get the mount path of volume: %s, err: %v", name, err)
		return "", "", errors.Wrap(err, "failed to get volume mount path")
	}

	return mountPath, driver, nil
}

func (mgr *ContainerManager) parseBinds(ctx context.Context, meta *ContainerMeta) error {
	logrus.Debugf("bind volumes: %v", meta.HostConfig.Binds)

	var err error

	if meta.Config.Volumes == nil {
		meta.Config.Volumes = make(map[string]interface{})
	}

	if meta.Mounts == nil {
		meta.Mounts = make([]*types.MountPoint, 0)
	}

	defer func() {
		if err != nil {
			if err := mgr.detachVolumes(ctx, meta); err != nil {
				logrus.Errorf("failed to detach volume, err: %v", err)
			}
		}
	}()

	// parse volumes from image
	image, err := mgr.ImageMgr.GetImage(ctx, meta.Image)
	if err != nil {
		return errors.Wrapf(err, "failed to get image: %s", meta.Image)
	}
	for dest := range image.Config.Volumes {
		name := randomid.Generate()
		if _, exist := meta.Config.Volumes[name]; exist {
			continue
		}

		if opts.CheckDuplicateMountPoint(meta.Mounts, dest) {
			logrus.Warnf("duplicate mount point: %s from image", dest)
			continue
		}

		mp := new(types.MountPoint)
		mp.Name = name
		mp.Named = true
		mp.Destination = dest

		mp.Source, mp.Driver, err = mgr.bindVolume(ctx, mp.Name, meta)
		if err != nil {
			logrus.Errorf("failed to bind volume: %s, err: %v", mp.Name, err)
			return errors.Wrap(err, "failed to bind volume")
		}

		err = opts.ParseBindMode(mp, "")
		if err != nil {
			logrus.Errorf("failed to parse mode, err: %v", err)
			return err
		}

		meta.Config.Volumes[mp.Name] = mp.Destination
		meta.Mounts = append(meta.Mounts, mp)
	}

	// parse binds
	for _, b := range meta.HostConfig.Binds {
		var parts []string
		parts, err = opts.CheckBind(b)
		if err != nil {
			return err
		}

		mode := ""
		mp := new(types.MountPoint)

		switch len(parts) {
		case 1:
			mp.Source = ""
			mp.Destination = parts[0]
		case 2:
			mp.Source = parts[0]
			mp.Destination = parts[1]
		case 3:
			mp.Source = parts[0]
			mp.Destination = parts[1]
			mode = parts[2]
		default:
			return errors.Errorf("unknown bind: %s", b)
		}

		if opts.CheckDuplicateMountPoint(meta.Mounts, mp.Destination) {
			logrus.Warnf("duplicate mount point: %s", mp.Destination)
			continue
		}

		if mp.Source == "" {
			mp.Source = randomid.Generate()
		}

		err = opts.ParseBindMode(mp, mode)
		if err != nil {
			logrus.Errorf("failed to parse bind mode: %s, err: %v", mode, err)
			return err
		}

		if !path.IsAbs(mp.Source) {
			// volume bind.
			name := mp.Source
			if _, exist := meta.Config.Volumes[name]; !exist {
				mp.Name = name
				mp.Source, mp.Driver, err = mgr.bindVolume(ctx, name, meta)
				if err != nil {
					logrus.Errorf("failed to bind volume: %s, err: %v", name, err)
					return errors.Wrap(err, "failed to bind volume")
				}
				meta.Config.Volumes[mp.Name] = mp.Destination
			}

			if mp.Replace != "" {
				mp.Source, err = mgr.VolumeMgr.Path(ctx, name)
				if err != nil {
					return err
				}

				switch mp.Replace {
				case "dr":
					mp.Source = path.Join(mp.Source, mp.Destination)
				case "rr":
					mp.Source = path.Join(mp.Source, randomid.Generate())
				}

				mp.Name = ""
				mp.Named = false
				mp.Driver = ""
			}
		}

		if _, err = os.Stat(mp.Source); err != nil {
			// host directory bind into container.
			if !os.IsNotExist(err) {
				return errors.Errorf("failed to stat %q: %v", mp.Source, err)
			}
			// Create the host path if it doesn't exist.
			if err = os.MkdirAll(mp.Source, 0755); err != nil {
				return errors.Errorf("failed to mkdir %q: %v", mp.Source, err)
			}
		}

		meta.Mounts = append(meta.Mounts, mp)
	}

	// parse volumes from other containers
	for _, v := range meta.HostConfig.VolumesFrom {
		var containerID, mode string
		containerID, mode, err = opts.ParseVolumesFrom(v)
		if err != nil {
			return err
		}

		var oldMeta *ContainerMeta
		oldMeta, err = mgr.Get(ctx, containerID)
		if err != nil {
			return err
		}

		for _, oldMountPoint := range oldMeta.Mounts {
			if opts.CheckDuplicateMountPoint(meta.Mounts, oldMountPoint.Destination) {
				logrus.Warnf("duplicate mount point: %s on container: %s", oldMountPoint.Destination, containerID)
				continue
			}

			mp := &types.MountPoint{
				Source:      oldMountPoint.Source,
				Destination: oldMountPoint.Destination,
				Driver:      oldMountPoint.Driver,
				Named:       oldMountPoint.Named,
				RW:          oldMountPoint.RW,
				Propagation: oldMountPoint.Propagation,
			}

			if _, exist := meta.Config.Volumes[oldMountPoint.Name]; len(oldMountPoint.Name) > 0 && !exist {
				mp.Name = oldMountPoint.Name
				mp.Source, mp.Driver, err = mgr.bindVolume(ctx, oldMountPoint.Name, meta)
				if err != nil {
					logrus.Errorf("failed to bind volume: %s, err: %v", oldMountPoint.Name, err)
					return errors.Wrap(err, "failed to bind volume")
				}
				meta.Config.Volumes[mp.Name] = oldMountPoint.Destination
			}

			err = opts.ParseBindMode(mp, mode)
			if err != nil {
				logrus.Errorf("failed to parse volumes-from mode: %s, err: %v", mode, err)
				return err
			}

			meta.Mounts = append(meta.Mounts, mp)
		}
	}

	return nil
}

func (mgr *ContainerManager) setMountPointDiskQuota(ctx context.Context, c *ContainerMeta) error {
	if c.Config.DiskQuota == nil {
		if c.Config.QuotaID != "" && c.Config.QuotaID != "0" {
			return fmt.Errorf("invalid argument, set quota-id without disk-quota")
		}
		return nil
	}

	var (
		qid        uint32
		setQuotaID bool
	)

	if c.Config.QuotaID != "" {
		id, err := strconv.Atoi(c.Config.QuotaID)
		if err != nil {
			return errors.Wrapf(err, "invalid argument, QuotaID: %s", c.Config.QuotaID)
		}

		// if QuotaID is < 0, it means pouchd alloc a unique quota id.
		if id < 0 {
			qid, err = quota.GetNextQuatoID()
			if err != nil {
				return errors.Wrap(err, "failed to get next quota id")
			}

			// update QuotaID
			c.Config.QuotaID = strconv.Itoa(int(qid))
		} else {
			qid = uint32(id)
		}
	}

	if qid > 0 {
		setQuotaID = true
	}

	// get rootfs quota
	quotas := c.Config.DiskQuota
	defaultQuota := quota.GetDefaultQuota(quotas)
	if setQuotaID && defaultQuota == "" {
		return fmt.Errorf("set quota id but have no set default quota size")
	}

	// parse diskquota regexe
	var res []*quota.RegExp
	for path, size := range quotas {
		re := regexp.MustCompile(path)
		res = append(res, &quota.RegExp{re, path, size})
	}

	for _, mp := range c.Mounts {
		// skip volume mount or replace mode mount
		if mp.Replace != "" || mp.Source == "" || mp.Destination == "" {
			logrus.Debugf("skip volume mount or replace mode mount")
			continue
		}

		if mp.Name != "" {
			v, err := mgr.VolumeMgr.Get(ctx, mp.Name)
			if err != nil {
				logrus.Warnf("failed to get volume: %s", mp.Name)
				continue
			}

			if v.Size() != "" {
				logrus.Debugf("skip volume: %s with size", mp.Name)
				continue
			}
		}

		// skip non-directory path.
		if fd, err := os.Stat(mp.Source); err != nil || !fd.IsDir() {
			logrus.Debugf("skip non-directory path: %s", mp.Source)
			continue
		}

		matched := false
		for _, re := range res {
			findStr := re.Pattern.FindString(mp.Destination)
			if findStr == mp.Destination {
				quotas[mp.Destination] = re.Size
				matched = true
				if re.Path != ".*" {
					break
				}
			}
		}

		size := ""
		if matched && !setQuotaID {
			size = quotas[mp.Destination]
		} else {
			size = defaultQuota
		}
		err := quota.SetDiskQuota(mp.Source, size, qid)
		if err != nil {
			return err
		}
	}

	c.Config.DiskQuota = quotas

	return nil
}

func (mgr *ContainerManager) detachVolumes(ctx context.Context, c *ContainerMeta) error {
	for name := range c.Config.Volumes {
		v, err := mgr.VolumeMgr.Get(ctx, name)
		if err != nil {
			logrus.Errorf("failed to get volume: %s", name)
			return err
		}

		option := map[string]string{}
		ref := v.Option("ref")
		if ref == "" {
			continue
		}
		if !strings.Contains(ref, c.ID) {
			continue
		}

		ids := strings.Split(ref, ",")
		for i, id := range ids {
			if id == c.ID {
				ids = append(ids[:i], ids[i+1:]...)
				break
			}
		}
		if len(ids) > 0 {
			option["ref"] = strings.Join(ids, ",")
		} else {
			option["ref"] = ""
		}

		mgr.VolumeMgr.Detach(ctx, name, option)
	}

	return nil
}

func (mgr *ContainerManager) buildContainerEndpoint(c *ContainerMeta) *networktypes.Endpoint {
	ep := &networktypes.Endpoint{
		Owner:           c.ID,
		Hostname:        c.Config.Hostname,
		Domainname:      c.Config.Domainname,
		HostsPath:       c.HostsPath,
		ExtraHosts:      c.HostConfig.ExtraHosts,
		HostnamePath:    c.HostnamePath,
		ResolvConfPath:  c.ResolvConfPath,
		NetworkDisabled: c.Config.NetworkDisabled,
		NetworkMode:     c.HostConfig.NetworkMode,
		DNS:             c.HostConfig.DNS,
		DNSOptions:      c.HostConfig.DNSOptions,
		DNSSearch:       c.HostConfig.DNSSearch,
		MacAddress:      c.Config.MacAddress,
		PublishAllPorts: c.HostConfig.PublishAllPorts,
		ExposedPorts:    c.Config.ExposedPorts,
		PortBindings:    c.HostConfig.PortBindings,
		NetworkConfig:   c.NetworkSettings,
	}

	if mgr.containerPlugin != nil {
		ep.Priority, ep.DisableResolver, ep.GenericParams = mgr.containerPlugin.PreCreateEndpoint(c.ID, c.Config.Env)
	}

	return ep
}

// setBaseFS keeps container basefs in meta
func (mgr *ContainerManager) setBaseFS(ctx context.Context, meta *ContainerMeta, container string) {
	info, err := mgr.Client.GetSnapshot(ctx, container)
	if err != nil {
		logrus.Infof("failed to get container %s snapshot", container)
		return
	}

	// io.containerd.runtime.v1.linux as a const used by runc
	meta.BaseFS = filepath.Join(mgr.Config.HomeDir, "containerd/state", "io.containerd.runtime.v1.linux", namespaces.Default, info.Name, "rootfs")
}

// execProcessGC cleans unused exec processes config every 5 minutes.
func (mgr *ContainerManager) execProcessGC() {
	for range time.Tick(time.Duration(GCExecProcessTick) * time.Minute) {
		execProcesses := mgr.ExecProcesses.Values()
		cleaned := 0

		for id, v := range execProcesses {
			execConfig, ok := v.(*ContainerExecConfig)
			if !ok {
				logrus.Warnf("get incorrect exec config: %v", v)
				continue
			}
			// if unused exec processes are found, we will tag them, and clean
			// them in next loop, so that we can ensure exec process can get
			// correct exit code.
			if execConfig.WaitForClean {
				cleaned++
				mgr.ExecProcesses.Remove(id)
			} else if !execConfig.Running {
				execConfig.WaitForClean = true
			}
		}

		if cleaned > 0 {
			logrus.Debugf("clean %d unused exec process", cleaned)
		}
	}
}
