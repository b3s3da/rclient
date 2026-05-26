package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"

	"rclient/internal/proto"
)

// metricsLoop samples and reports system metrics on a fixed interval.
func (a *Agent) metricsLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.MetricsEvery)
	defer t.Stop()
	// Send one immediately so the panel shows data right after connect.
	a.sendMetricsOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.sendMetricsOnce(ctx)
		}
	}
}

func (a *Agent) sendMetricsOnce(ctx context.Context) {
	m := collectMetrics(ctx)
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	a.send(ctx, proto.Envelope{Type: proto.TypeMetrics, Data: raw})
}

func collectMetrics(ctx context.Context) proto.Metrics {
	m := proto.Metrics{TS: time.Now().Unix()}

	// CPU percent over a 1s window. gopsutil also has a non-blocking variant
	// that compares against the previous call; the sampled version is simpler
	// and accurate enough for our use.
	if pcts, err := cpu.PercentWithContext(ctx, time.Second, false); err == nil && len(pcts) > 0 {
		m.CPUPercent = pcts[0]
	}

	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		m.MemPercent = v.UsedPercent
		m.MemUsedMB = v.Used / 1024 / 1024
		m.MemTotalMB = v.Total / 1024 / 1024
	}

	if d, err := disk.UsageWithContext(ctx, "/"); err == nil {
		m.DiskPct = d.UsedPercent
	}

	if up, err := host.UptimeWithContext(ctx); err == nil {
		m.UptimeSec = up
	}

	if avg, err := load.AvgWithContext(ctx); err == nil {
		m.Load1 = avg.Load1
		m.Load5 = avg.Load5
		m.Load15 = avg.Load15
	}

	return m
}

// kernelVersion returns the kernel/platform string for the Hello message.
func kernelVersion() string {
	if info, err := host.Info(); err == nil {
		return info.KernelVersion
	}
	return ""
}
