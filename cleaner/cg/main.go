package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
)

func init() {
	if *debugFlag {
		log.SetLevel(log.DebugLevel)
		log.SetReportCaller(true)
	}
	log.SetFormatter(&logrus.TextFormatter{
		ForceColors: true,
	})
}

// CLI flags
var (
	strainFlag = flag.Float64("strain", 20, "maximum amount of strain per CPU core")
	pathFlag   = flag.String("path", "/sf/config/db/cg", "directory path where action logs are stored")
	debugFlag  = flag.Bool("debug", false, "activate debug mode")
)

func main() {
	flag.Parse()

	log.Infof("ContainerGuard (CG) started protecting your Segfault.Net instance...")

	// docker client
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}
	log.Debugf("connected to docker client v%v", cli.ClientVersion())

	// number of virtual cores
	var numCPU = runtime.NumCPU()
	// MAX_LOAD defines the maximum amount of `strain` each CPU can have
	// before triggering our cleanup tasks.
	var MAX_LOAD = *strainFlag * float64(numCPU)
	// last recorded loadavg after a trigger event
	var LAST_LOAD float64 // default value 0.0
	var loop_time_s = 5

	var count int
	for range time.Tick(time.Second * time.Duration(loop_time_s)) {

		// protect legitimate users
		if LAST_LOAD != 0.0 { // we got a trigger event
			// after 60s stop protecting
			if count > 60/loop_time_s {
				LAST_LOAD = 0.0
				count = 0
				continue
			}

			if sysLoad1mAvg() <= LAST_LOAD {
				LAST_LOAD = sysLoad1mAvg()
				count++
				continue
			}

			// if load doesn't go down every 5s
			LAST_LOAD = 0.0 // reset
		}

		if sysLoad1mAvg() <= MAX_LOAD {
			continue
		}

		log.Warnf("[TRIGGER] load (%.2f) on cpu (%v) higher than max_load (%v)", sysLoad1mAvg(), numCPU, MAX_LOAD)
		LAST_LOAD = sysLoad1mAvg()
		err = stopContainersBasedOnUsage(cli)
		if err != nil {
			log.Error(err)
		}
	}
}

// stopContainersBasedOnUsage iterates through all the containers on the system
// to find abusive ones and stops them, but only if their name starts w/ lg-*
func stopContainersBasedOnUsage(cli *client.Client) error {
	const filterPrefix = "/lg-*"
	opts := types.ContainerListOptions{}
	opts.All = false // list only running containers
	opts.Filters = filters.NewArgs()
	opts.Filters.Add("name", filterPrefix)

	ctx := context.Background()
	list, err := cli.ContainerList(ctx, opts)
	if err != nil {
		return err
	}

	// mu protects `_largestUsage`
	var mu sync.Mutex
	var highestUsage float64

	// used to synchronize goroutines
	var wg = &sync.WaitGroup{}

	// check all containers usage and keep largest value in `largestUsage` var
	for _, c := range list {
		wg.Add(1)
		go func(c types.Container) {
			defer wg.Done()

			usage := containerUsage(cli, c.ID)
			if usage > highestUsage {
				mu.Lock()
				highestUsage = usage
				mu.Unlock()
			}
			log.Infof("[%v] usage (%.2f%%)", c.Names[0][1:], usage)
		}(c)
	}
	wg.Wait()
	log.Infof("[HIGHEST USAGE] %.2f%%", highestUsage)

	for _, c := range list {
		wg.Add(1)
		go func(c types.Container) {
			defer wg.Done()

			usage := containerUsage(cli, c.ID)
			log.Debugf("allowed to kill %v with usage %v", c.Names[0], usage)

			var killTimeout = time.Second * 2
			var killThreshold = highestUsage * 0.8
			const action = "STOP in 2s or KILL"

			// stop all containers where usage > `highestUsage` * 0.8
			if usage > killThreshold {
				log.Warnf("[%v] usage (%.2f%%) > threshold (%.2f%%) | action %v", c.Names[0][1:], usage, killThreshold, action)

				ctx := context.Background()
				err := cli.ContainerStop(ctx, c.ID, &killTimeout)
				if err != nil {
					log.Error(err)
					return
				}
			}

			// log stopped containers to disk
			logData := LogData{
				name:      c.Names[0],
				usage:     usage,
				threshold: killThreshold,
				load:      sysLoad1mAvg(),
				action:    action,
			}
			if err := logData.save(*pathFlag); err != nil {
				log.Error(err)
				return
			}
		}(c)
	}
	wg.Wait()

	return nil
}

// containerUsage calculates the CPU usage of a container.
func containerUsage(cli *client.Client, cID string) float64 {
	ctx := context.Background()
	stats, err := cli.ContainerStats(ctx, cID, false)
	if err != nil {
		log.Error(err)
		return 0
	}
	defer stats.Body.Close()

	var result ContainerStatsData
	err = json.NewDecoder(stats.Body).Decode(&result)
	if err != nil {
		log.Error(err)
		b, _ := ioutil.ReadAll(stats.Body)
		log.Error(b)
	}

	// https://github.com/docker/cli/blob/53f8ed4bec07084db4208f55987a2ea94b7f01d6/cli/command/container/stats_helpers.go#L166
	// calculations
	cpu_delta := float64(result.CPUStats.CPUUsage.TotalUsage) - float64(result.PrecpuStats.CPUUsage.TotalUsage)
	system_cpu_delta := result.CPUStats.SystemCPUUsage - result.PrecpuStats.SystemCPUUsage
	number_cpus := result.CPUStats.OnlineCpus
	usage := (float64(cpu_delta) / float64(system_cpu_delta)) * float64(number_cpus) * 100.0

	return usage
}

type LogData struct {
	name      string
	usage     float64
	threshold float64
	load      float64
	action    string
}

// run mkdir only once
var mkdirOnce = sync.Once{}

func (a LogData) save(path string) error {

	var err error
	mkdirOnce.Do(func() {
		err = os.MkdirAll(path, 0770)
	})
	if err != nil {
		return err
	}

	t := time.Now().UTC().Unix()
	// example: 1666389757 usage=95.71 threshold=28.61 load=200.23 action=SIGKILL
	data := fmt.Sprintf("%v usage=%.2f threshold=%.2f load=%.2f action=%s ", t, a.usage, a.threshold, a.load, a.action)
	filePath := filepath.Join(path, a.name+".txt")

	if err := os.WriteFile(filePath, []byte(data), 0660); err != nil {
		return err
	}

	log.Debugf("[LOG FILE] %v", filePath)

	return nil
}

type ContainerStatsData struct {
	CPUStats struct {
		CPUUsage struct {
			UsageInUsermode   int `json:"usage_in_usermode"`
			TotalUsage        int `json:"total_usage"`
			UsageInKernelmode int `json:"usage_in_kernelmode"`
		} `json:"cpu_usage"`
		SystemCPUUsage int `json:"system_cpu_usage"`
		OnlineCpus     int `json:"online_cpus"`
		ThrottlingData struct {
			Periods          int `json:"periods"`
			ThrottledPeriods int `json:"throttled_periods"`
			ThrottledTime    int `json:"throttled_time"`
		} `json:"throttling_data"`
	} `json:"cpu_stats"`
	PrecpuStats struct {
		CPUUsage struct {
			PercpuUsage       []int `json:"percpu_usage"`
			UsageInUsermode   int   `json:"usage_in_usermode"`
			TotalUsage        int   `json:"total_usage"`
			UsageInKernelmode int   `json:"usage_in_kernelmode"`
		} `json:"cpu_usage"`
		SystemCPUUsage int `json:"system_cpu_usage"`
		OnlineCpus     int `json:"online_cpus"`
		ThrottlingData struct {
			Periods          int `json:"periods"`
			ThrottledPeriods int `json:"throttled_periods"`
			ThrottledTime    int `json:"throttled_time"`
		} `json:"throttling_data"`
	} `json:"precpu_stats"`
}
