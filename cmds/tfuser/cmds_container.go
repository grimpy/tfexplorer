package main

import (
	"fmt"
	"net"
	"strings"

	"github.com/pkg/errors"
	"github.com/threefoldtech/tfexplorer/models/generated/workloads"
	"github.com/threefoldtech/tfexplorer/provision/builders"
	"github.com/threefoldtech/zos/pkg/container/logger"
	"github.com/threefoldtech/zos/pkg/container/stats"
	"github.com/urfave/cli"
)

func generateContainer(c *cli.Context) error {

	envs, err := splitEnvs(c.StringSlice("envs"))
	if err != nil {
		return err
	}

	mounts, err := splitMounts(c.StringSlice("mounts"))
	if err != nil {
		return err
	}

	cap := workloads.ContainerCapacity{
		Cpu:      c.Int64("cpu"),
		Memory:   c.Int64("memory"),
		DiskSize: c.Uint64("disk-size"),
	}
	switch strings.ToLower(c.String("disk-type")) {
	case "hdd":
		cap.DiskType = workloads.DiskTypeHDD
	case "ssd":
		cap.DiskType = workloads.DiskTypeSSD
	}

	var sts []workloads.StatsAggregator
	if s := c.String("stats"); s != "" {
		// validating stdout argument
		_, _, err := logger.RedisParseURL(s)
		if err != nil {
			return err
		}

		ss := workloads.StatsAggregator{
			Type: stats.RedisType,
			Data: workloads.StatsRedis{
				Endpoint: s,
			},
		}

		sts = append(sts, ss)
	}

	var logs []workloads.Logs
	if lo := c.String("stdout"); lo != "" {
		// validating stdout argument
		_, _, err := logger.RedisParseURL(lo)
		if err != nil {
			return err
		}

		// copy stdout to stderr
		lr := lo

		// check if stderr is specified
		if nlr := c.String("stderr"); nlr != "" {
			// validating stderr argument
			_, _, err := logger.RedisParseURL(nlr)
			if err != nil {
				return nil
			}

			lr = nlr
		}

		lg := workloads.Logs{
			Type: "redis",
			Data: workloads.LogsRedis{
				Stdout: lo,
				Stderr: lr,
			},
		}

		logs = append(logs, lg)
	}

	network := []workloads.NetworkConnection{
		workloads.NetworkConnection{
			NetworkId: c.String("network"),
			Ipaddress: net.ParseIP(c.String("ip")),
			PublicIp6: c.Bool("public6"),
		},
	}

	containerBuilder := builders.NewContainerBuilder(c.String("node"), c.String("flist"), network)

	containerBuilder.
		WithEnvs(envs).
		WithEntrypoint(c.String("entrypoint")).
		WithVolumes(mounts).WithInteractive(c.Bool("corex")).
		WithContainerCapacity(cap).
		WithLogs(logs).
		WithStatsAggregator(sts)

	container, err := containerBuilder.Build()
	if err != nil {
		return errors.Wrap(err, "failed to build container")
	}
	return writeWorkload(c.GlobalString("output"), container)
}

func splitEnvs(envs []string) (map[string]string, error) {
	out := make(map[string]string, len(envs))

	for _, env := range envs {
		ss := strings.SplitN(env, "=", 2)
		if len(ss) != 2 {
			return nil, fmt.Errorf("envs flag mal formatted: %v", env)
		}
		out[ss[0]] = ss[1]
	}

	return out, nil
}

func splitMounts(mounts []string) ([]workloads.ContainerMount, error) {
	out := make([]workloads.ContainerMount, 0, len(mounts))

	for _, mount := range mounts {
		ss := strings.SplitN(mount, ":", 2)
		if len(ss) != 2 {
			return nil, fmt.Errorf("mounts flag mal formatted: %v", mount)
		}

		out = append(out, workloads.ContainerMount{
			VolumeId:   ss[0],
			Mountpoint: ss[1],
		})
	}

	return out, nil
}
