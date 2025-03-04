package common

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containers/common/pkg/cgroups"
	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v3/cmd/podman/registry"
	"github.com/containers/podman/v3/libpod/network/types"
	"github.com/containers/podman/v3/pkg/api/handlers"
	"github.com/containers/podman/v3/pkg/domain/entities"
	"github.com/containers/podman/v3/pkg/rootless"
	"github.com/containers/podman/v3/pkg/specgen"
	"github.com/docker/docker/api/types/mount"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func stringMaptoArray(m map[string]string) []string {
	a := make([]string, 0, len(m))
	for k, v := range m {
		a = append(a, fmt.Sprintf("%s=%s", k, v))
	}
	return a
}

// ContainerCreateToContainerCLIOpts converts a compat input struct to cliopts so it can be converted to
// a specgen spec.
func ContainerCreateToContainerCLIOpts(cc handlers.CreateContainerConfig, rtc *config.Config) (*entities.ContainerCreateOptions, []string, error) {
	var (
		capAdd     []string
		cappDrop   []string
		entrypoint *string
		init       bool
		specPorts  []types.PortMapping
	)

	if cc.HostConfig.Init != nil {
		init = *cc.HostConfig.Init
	}

	// Iterate devices and convert back to string
	devices := make([]string, 0, len(cc.HostConfig.Devices))
	for _, dev := range cc.HostConfig.Devices {
		devices = append(devices, fmt.Sprintf("%s:%s:%s", dev.PathOnHost, dev.PathInContainer, dev.CgroupPermissions))
	}

	// iterate blkreaddevicebps
	readBps := make([]string, 0, len(cc.HostConfig.BlkioDeviceReadBps))
	for _, dev := range cc.HostConfig.BlkioDeviceReadBps {
		readBps = append(readBps, dev.String())
	}

	// iterate blkreaddeviceiops
	readIops := make([]string, 0, len(cc.HostConfig.BlkioDeviceReadIOps))
	for _, dev := range cc.HostConfig.BlkioDeviceReadIOps {
		readIops = append(readIops, dev.String())
	}

	// iterate blkwritedevicebps
	writeBps := make([]string, 0, len(cc.HostConfig.BlkioDeviceWriteBps))
	for _, dev := range cc.HostConfig.BlkioDeviceWriteBps {
		writeBps = append(writeBps, dev.String())
	}

	// iterate blkwritedeviceiops
	writeIops := make([]string, 0, len(cc.HostConfig.BlkioDeviceWriteIOps))
	for _, dev := range cc.HostConfig.BlkioDeviceWriteIOps {
		writeIops = append(writeIops, dev.String())
	}

	// entrypoint
	// can be a string or slice. if it is a slice, we need to
	// marshall it to json; otherwise it should just be the string
	// value
	if len(cc.Config.Entrypoint) > 0 {
		entrypoint = &cc.Config.Entrypoint[0]
		if len(cc.Config.Entrypoint) > 1 {
			b, err := json.Marshal(cc.Config.Entrypoint)
			if err != nil {
				return nil, nil, err
			}
			var jsonString = string(b)
			entrypoint = &jsonString
		}
	}

	// expose ports
	expose := make([]string, 0, len(cc.Config.ExposedPorts))
	for p := range cc.Config.ExposedPorts {
		expose = append(expose, fmt.Sprintf("%s/%s", p.Port(), p.Proto()))
	}

	// mounts type=tmpfs/bind,source=...,target=...=,opt=val
	mounts := make([]string, 0, len(cc.HostConfig.Mounts))
	var builder strings.Builder
	for _, m := range cc.HostConfig.Mounts {
		addField(&builder, "type", string(m.Type))
		addField(&builder, "source", m.Source)
		addField(&builder, "target", m.Target)
		if m.ReadOnly {
			addField(&builder, "ro", "true")
		}
		addField(&builder, "consistency", string(m.Consistency))
		// Map any specialized mount options that intersect between *Options and cli options
		switch m.Type {
		case mount.TypeBind:
			if m.BindOptions != nil {
				addField(&builder, "bind-propagation", string(m.BindOptions.Propagation))
				addField(&builder, "bind-nonrecursive", strconv.FormatBool(m.BindOptions.NonRecursive))
			}
		case mount.TypeTmpfs:
			if m.TmpfsOptions != nil {
				addField(&builder, "tmpfs-size", strconv.FormatInt(m.TmpfsOptions.SizeBytes, 10))
				addField(&builder, "tmpfs-mode", strconv.FormatUint(uint64(m.TmpfsOptions.Mode), 10))
			}
		case mount.TypeVolume:
			// All current VolumeOpts are handled above
			// See vendor/github.com/containers/common/pkg/parse/parse.go:ValidateVolumeOpts()
		}
		mounts = append(mounts, builder.String())
		builder.Reset()
	}

	// dns
	dns := make([]net.IP, 0, len(cc.HostConfig.DNS))
	for _, d := range cc.HostConfig.DNS {
		dns = append(dns, net.ParseIP(d))
	}

	// publish
	for port, pbs := range cc.HostConfig.PortBindings {
		for _, pb := range pbs {
			var hostport int
			var err error
			if pb.HostPort != "" {
				hostport, err = strconv.Atoi(pb.HostPort)
			}
			if err != nil {
				return nil, nil, err
			}
			tmpPort := types.PortMapping{
				HostIP:        pb.HostIP,
				ContainerPort: uint16(port.Int()),
				HostPort:      uint16(hostport),
				Range:         0,
				Protocol:      port.Proto(),
			}
			specPorts = append(specPorts, tmpPort)
		}
	}

	// netMode
	nsmode, networks, netOpts, err := specgen.ParseNetworkFlag([]string{string(cc.HostConfig.NetworkMode)})
	if err != nil {
		return nil, nil, err
	}

	// network
	// Note: we cannot emulate compat exactly here. we only allow specifics of networks to be
	// defined when there is only one network.
	netInfo := entities.NetOptions{
		AddHosts:       cc.HostConfig.ExtraHosts,
		DNSOptions:     cc.HostConfig.DNSOptions,
		DNSSearch:      cc.HostConfig.DNSSearch,
		DNSServers:     dns,
		Network:        nsmode,
		PublishPorts:   specPorts,
		NetworkOptions: netOpts,
	}

	// network names
	switch {
	case len(cc.NetworkingConfig.EndpointsConfig) > 0:
		endpointsConfig := cc.NetworkingConfig.EndpointsConfig
		networks := make(map[string]types.PerNetworkOptions, len(endpointsConfig))
		for netName, endpoint := range endpointsConfig {
			netOpts := types.PerNetworkOptions{}
			if endpoint != nil {
				netOpts.Aliases = endpoint.Aliases

				// if IP address is provided
				if len(endpoint.IPAddress) > 0 {
					staticIP := net.ParseIP(endpoint.IPAddress)
					if staticIP == nil {
						return nil, nil, errors.Errorf("failed to parse the ip address %q", endpoint.IPAddress)
					}
					netOpts.StaticIPs = append(netOpts.StaticIPs, staticIP)
				}

				if endpoint.IPAMConfig != nil {
					// if IPAMConfig.IPv4Address is provided
					if len(endpoint.IPAMConfig.IPv4Address) > 0 {
						staticIP := net.ParseIP(endpoint.IPAMConfig.IPv4Address)
						if staticIP == nil {
							return nil, nil, errors.Errorf("failed to parse the ipv4 address %q", endpoint.IPAMConfig.IPv4Address)
						}
						netOpts.StaticIPs = append(netOpts.StaticIPs, staticIP)
					}
					// if IPAMConfig.IPv6Address is provided
					if len(endpoint.IPAMConfig.IPv6Address) > 0 {
						staticIP := net.ParseIP(endpoint.IPAMConfig.IPv6Address)
						if staticIP == nil {
							return nil, nil, errors.Errorf("failed to parse the ipv6 address %q", endpoint.IPAMConfig.IPv6Address)
						}
						netOpts.StaticIPs = append(netOpts.StaticIPs, staticIP)
					}
				}
				// If MAC address is provided
				if len(endpoint.MacAddress) > 0 {
					staticMac, err := net.ParseMAC(endpoint.MacAddress)
					if err != nil {
						return nil, nil, errors.Errorf("failed to parse the mac address %q", endpoint.MacAddress)
					}
					netOpts.StaticMAC = types.HardwareAddr(staticMac)
				}
			}

			networks[netName] = netOpts
		}

		netInfo.Networks = networks
	case len(cc.HostConfig.NetworkMode) > 0:
		netInfo.Networks = networks
	}

	parsedTmp := make([]string, 0, len(cc.HostConfig.Tmpfs))
	for path, options := range cc.HostConfig.Tmpfs {
		finalString := path
		if options != "" {
			finalString += ":" + options
		}
		parsedTmp = append(parsedTmp, finalString)
	}

	// Note: several options here are marked as "don't need". this is based
	// on speculation by Matt and I. We think that these come into play later
	// like with start. We believe this is just a difference in podman/compat
	cliOpts := entities.ContainerCreateOptions{
		// Attach:            nil, // don't need?
		Authfile:     "",
		CapAdd:       append(capAdd, cc.HostConfig.CapAdd...),
		CapDrop:      append(cappDrop, cc.HostConfig.CapDrop...),
		CGroupParent: cc.HostConfig.CgroupParent,
		CIDFile:      cc.HostConfig.ContainerIDFile,
		CPUPeriod:    uint64(cc.HostConfig.CPUPeriod),
		CPUQuota:     cc.HostConfig.CPUQuota,
		CPURTPeriod:  uint64(cc.HostConfig.CPURealtimePeriod),
		CPURTRuntime: cc.HostConfig.CPURealtimeRuntime,
		CPUShares:    uint64(cc.HostConfig.CPUShares),
		// CPUS:              0, // don't need?
		CPUSetCPUs: cc.HostConfig.CpusetCpus,
		CPUSetMems: cc.HostConfig.CpusetMems,
		// Detach:            false, // don't need
		// DetachKeys:        "",    // don't need
		Devices:           devices,
		DeviceCGroupRule:  nil,
		DeviceReadBPs:     readBps,
		DeviceReadIOPs:    readIops,
		DeviceWriteBPs:    writeBps,
		DeviceWriteIOPs:   writeIops,
		Entrypoint:        entrypoint,
		Env:               cc.Config.Env,
		Expose:            expose,
		GroupAdd:          cc.HostConfig.GroupAdd,
		Hostname:          cc.Config.Hostname,
		ImageVolume:       "bind",
		Init:              init,
		Interactive:       cc.Config.OpenStdin,
		IPC:               string(cc.HostConfig.IpcMode),
		Label:             stringMaptoArray(cc.Config.Labels),
		LogDriver:         cc.HostConfig.LogConfig.Type,
		LogOptions:        stringMaptoArray(cc.HostConfig.LogConfig.Config),
		Name:              cc.Name,
		OOMScoreAdj:       cc.HostConfig.OomScoreAdj,
		Arch:              "",
		OS:                "",
		Variant:           "",
		PID:               string(cc.HostConfig.PidMode),
		PIDsLimit:         cc.HostConfig.PidsLimit,
		Privileged:        cc.HostConfig.Privileged,
		PublishAll:        cc.HostConfig.PublishAllPorts,
		Quiet:             false,
		ReadOnly:          cc.HostConfig.ReadonlyRootfs,
		ReadOnlyTmpFS:     true, // podman default
		Rm:                cc.HostConfig.AutoRemove,
		SecurityOpt:       cc.HostConfig.SecurityOpt,
		StopSignal:        cc.Config.StopSignal,
		StorageOpts:       stringMaptoArray(cc.HostConfig.StorageOpt),
		Sysctl:            stringMaptoArray(cc.HostConfig.Sysctls),
		Systemd:           "true", // podman default
		TmpFS:             parsedTmp,
		TTY:               cc.Config.Tty,
		UnsetEnv:          cc.UnsetEnv,
		UnsetEnvAll:       cc.UnsetEnvAll,
		User:              cc.Config.User,
		UserNS:            string(cc.HostConfig.UsernsMode),
		UTS:               string(cc.HostConfig.UTSMode),
		Mount:             mounts,
		VolumesFrom:       cc.HostConfig.VolumesFrom,
		Workdir:           cc.Config.WorkingDir,
		Net:               &netInfo,
		HealthInterval:    DefaultHealthCheckInterval,
		HealthRetries:     DefaultHealthCheckRetries,
		HealthTimeout:     DefaultHealthCheckTimeout,
		HealthStartPeriod: DefaultHealthCheckStartPeriod,
	}
	if !rootless.IsRootless() {
		var ulimits []string
		if len(cc.HostConfig.Ulimits) > 0 {
			for _, ul := range cc.HostConfig.Ulimits {
				ulimits = append(ulimits, ul.String())
			}
			cliOpts.Ulimit = ulimits
		}
	}
	if cc.HostConfig.Resources.NanoCPUs > 0 {
		if cliOpts.CPUPeriod != 0 || cliOpts.CPUQuota != 0 {
			return nil, nil, errors.Errorf("NanoCpus conflicts with CpuPeriod and CpuQuota")
		}
		cliOpts.CPUPeriod = 100000
		cliOpts.CPUQuota = cc.HostConfig.Resources.NanoCPUs / 10000
	}

	// volumes
	volSources := make(map[string]bool)
	volDestinations := make(map[string]bool)
	for _, vol := range cc.HostConfig.Binds {
		cliOpts.Volume = append(cliOpts.Volume, vol)
		// Extract the destination so we don't add duplicate mounts in
		// the volumes phase.
		splitVol := strings.SplitN(vol, ":", 3)
		switch len(splitVol) {
		case 1:
			volDestinations[vol] = true
		default:
			volSources[splitVol[0]] = true
			volDestinations[splitVol[1]] = true
		}
	}
	// Anonymous volumes are added differently from other volumes, in their
	// own special field, for reasons known only to Docker. Still use the
	// format of `-v` so we can just append them in there.
	// Unfortunately, these may be duplicates of existing mounts in Binds.
	// So... We need to catch that.
	for vol := range cc.Volumes {
		if _, ok := volDestinations[filepath.Clean(vol)]; ok {
			continue
		}
		cliOpts.Volume = append(cliOpts.Volume, vol)
	}
	// Make mount points for compat volumes
	for vol := range volSources {
		// This might be a named volume.
		// Assume it is if it's not an absolute path.
		if !filepath.IsAbs(vol) {
			continue
		}
		// If volume already exists, there is nothing to do
		if _, err := os.Stat(vol); err == nil {
			continue
		}
		if err := os.MkdirAll(vol, 0755); err != nil {
			if !os.IsExist(err) {
				return nil, nil, errors.Wrapf(err, "error making volume mountpoint for volume %s", vol)
			}
		}
	}
	if len(cc.HostConfig.BlkioWeightDevice) > 0 {
		devices := make([]string, 0, len(cc.HostConfig.BlkioWeightDevice))
		for _, d := range cc.HostConfig.BlkioWeightDevice {
			devices = append(devices, d.String())
		}
		cliOpts.BlkIOWeightDevice = devices
	}
	if cc.HostConfig.BlkioWeight > 0 {
		cliOpts.BlkIOWeight = strconv.Itoa(int(cc.HostConfig.BlkioWeight))
	}

	if cc.HostConfig.Memory > 0 {
		cliOpts.Memory = strconv.Itoa(int(cc.HostConfig.Memory))
	}
	if cc.HostConfig.KernelMemory > 0 {
		logrus.Warnf("The --kernel-memory flag has been deprecated. May not work properly on your system.")
	}

	if cc.HostConfig.MemoryReservation > 0 {
		cliOpts.MemoryReservation = strconv.Itoa(int(cc.HostConfig.MemoryReservation))
	}

	cgroupsv2, err := cgroups.IsCgroup2UnifiedMode()
	if err != nil {
		return nil, nil, err
	}
	if cc.HostConfig.MemorySwap > 0 && (!rootless.IsRootless() || (rootless.IsRootless() && cgroupsv2)) {
		cliOpts.MemorySwap = strconv.Itoa(int(cc.HostConfig.MemorySwap))
	}

	if cc.Config.StopTimeout != nil {
		cliOpts.StopTimeout = uint(*cc.Config.StopTimeout)
	}

	if cc.HostConfig.ShmSize > 0 {
		cliOpts.ShmSize = strconv.Itoa(int(cc.HostConfig.ShmSize))
	}

	if cc.HostConfig.KernelMemory > 0 {
		cliOpts.KernelMemory = strconv.Itoa(int(cc.HostConfig.KernelMemory))
	}
	if len(cc.HostConfig.RestartPolicy.Name) > 0 {
		policy := cc.HostConfig.RestartPolicy.Name
		// only add restart count on failure
		if cc.HostConfig.RestartPolicy.IsOnFailure() {
			policy += fmt.Sprintf(":%d", cc.HostConfig.RestartPolicy.MaximumRetryCount)
		}
		cliOpts.Restart = policy
	}

	if cc.HostConfig.MemorySwappiness != nil && (!rootless.IsRootless() || rootless.IsRootless() && cgroupsv2 && rtc.Engine.CgroupManager == "systemd") {
		cliOpts.MemorySwappiness = *cc.HostConfig.MemorySwappiness
	} else {
		cliOpts.MemorySwappiness = -1
	}
	if cc.HostConfig.OomKillDisable != nil {
		cliOpts.OOMKillDisable = *cc.HostConfig.OomKillDisable
	}
	if cc.Config.Healthcheck != nil {
		finCmd := ""
		for _, str := range cc.Config.Healthcheck.Test {
			finCmd = finCmd + str + " "
		}
		if len(finCmd) > 1 {
			finCmd = finCmd[:len(finCmd)-1]
		}
		cliOpts.HealthCmd = finCmd
		if cc.Config.Healthcheck.Interval > 0 {
			cliOpts.HealthInterval = cc.Config.Healthcheck.Interval.String()
		}
		if cc.Config.Healthcheck.Retries > 0 {
			cliOpts.HealthRetries = uint(cc.Config.Healthcheck.Retries)
		}
		if cc.Config.Healthcheck.StartPeriod > 0 {
			cliOpts.HealthStartPeriod = cc.Config.Healthcheck.StartPeriod.String()
		}
		if cc.Config.Healthcheck.Timeout > 0 {
			cliOpts.HealthTimeout = cc.Config.Healthcheck.Timeout.String()
		}
	}

	// specgen assumes the image name is arg[0]
	cmd := []string{cc.Config.Image}
	cmd = append(cmd, cc.Config.Cmd...)
	return &cliOpts, cmd, nil
}

func ulimits() []string {
	if !registry.IsRemote() {
		return containerConfig.Ulimits()
	}
	return nil
}

func cgroupConfig() string {
	if !registry.IsRemote() {
		return containerConfig.Cgroups()
	}
	return ""
}

func devices() []string {
	if !registry.IsRemote() {
		return containerConfig.Devices()
	}
	return nil
}

func env() []string {
	if !registry.IsRemote() {
		return containerConfig.Env()
	}
	return nil
}

func initPath() string {
	if !registry.IsRemote() {
		return containerConfig.InitPath()
	}
	return ""
}

func pidsLimit() int64 {
	if !registry.IsRemote() {
		return containerConfig.PidsLimit()
	}
	return -1
}

func policy() string {
	if !registry.IsRemote() {
		return containerConfig.Engine.PullPolicy
	}
	return ""
}

func shmSize() string {
	if !registry.IsRemote() {
		return containerConfig.ShmSize()
	}
	return ""
}

func volumes() []string {
	if !registry.IsRemote() {
		return containerConfig.Volumes()
	}
	return nil
}

func logDriver() string {
	if !registry.IsRemote() {
		return containerConfig.Containers.LogDriver
	}
	return ""
}

// addField is a helper function to populate mount options
func addField(b *strings.Builder, name string, value string) {
	if value == "" {
		return
	}

	if b.Len() > 0 {
		b.WriteRune(',')
	}
	b.WriteString(name)
	b.WriteRune('=')
	b.WriteString(value)
}
