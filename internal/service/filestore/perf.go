package filestore

import (
	"math"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
)

// Performance limits (#2194), as
// https://docs.cloud.google.com/filestore/docs/performance and
// https://docs.cloud.google.com/filestore/docs/custom-performance document
// them (read 2026-11-23). The backend enforces them: each instance's
// server may read and write no more than its limits (IOPS and bytes a
// second), whatever its clients ask.
//
// Capacity-based limits (no performance_config), per TiB and scaled
// linearly with capacity, as the capacity-based table's rows are:
//
//   - ZONAL, 1 TiB to 9.99 TiB and 10 TiB to 100 TiB, and REGIONAL and
//     HIGH_SCALE_SSD's 10 TiB to 100 TiB: 9,200 read IOPS, 2,600 write
//     IOPS, 260 MiBps read, 88 MiBps write a TiB;
//   - REGIONAL below 10 TiB and ENTERPRISE: 12,000 read IOPS, 4,000 write
//     IOPS, 120 MiBps read, 100 MiBps write a TiB;
//   - BASIC_HDD: 600 read IOPS, 1,000 write IOPS, 100 MiBps each way from
//     1 TiB to 10 TiB; 1,000, 5,000, 180 and 120 from 10 TiB to 63.9 TiB;
//   - BASIC_SSD: 60,000 read IOPS, 25,000 write IOPS, 1,200 MiBps read,
//     350 MiBps write at every capacity.
//
// Custom performance (ZONAL and REGIONAL): max_iops is iops_per_tb times
// the capacity in TiB, or fixed_iops, at least 2,000 below 1 TiB ("Instances
// with capacity lower than 1,024 GiB require custom performance with a
// minimum of 2,000 IOPS"); "each read operation takes one operation from
// the purchased IOPS, while write operation takes 3.33 IOPS", so the write
// IOPS are 30% of max_iops; the throughputs are 24 KiB (read) and 8 KiB
// (write) a purchased IOPS, as the page's fixed-IOPS sample answers
// (max_iops 9000: max_read_throughput_bps 221184000,
// max_write_throughput_bps 73728000). A ZONAL or REGIONAL instance below
// 1 TiB without performance_config takes the lowest custom performance,
// 4,000 IOPS a TiB (UNVERIFIED, re-read 2026-11-23: the page says only that custom performance
// cannot be turned off there).

const mib = 1 << 20

type perTiB struct{ readIOPS, writeIOPS, readMiB, writeMiB float64 }

var (
	zonalPerf      = perTiB{9200, 2600, 260, 88}
	enterprisePerf = perTiB{12000, 4000, 120, 100}
)

// customOf is a custom performance's max_iops, or 0 without one.
func customOf(pc *filestorepb.Instance_PerformanceConfig, capacityGB int64) int64 {
	var iops int64
	switch {
	case pc.GetIopsPerTb() != nil:
		iops = int64(math.Round(float64(pc.GetIopsPerTb().GetMaxIopsPerTb()) * float64(capacityGB) / 1024))
	case pc.GetFixedIops() != nil:
		iops = pc.GetFixedIops().GetMaxIops()
	default:
		return 0
	}
	if capacityGB < 1024 && iops < 2000 {
		iops = 2000
	}
	return iops
}

// PerformanceLimits are an instance's documented limits.
func PerformanceLimits(tier filestorepb.Instance_Tier, capacityGB int64, pc *filestorepb.Instance_PerformanceConfig) *filestorepb.Instance_PerformanceLimits {
	custom := tier == filestorepb.Instance_ZONAL || tier == filestorepb.Instance_REGIONAL
	iops := int64(0)
	if custom {
		iops = customOf(pc, capacityGB)
		if iops == 0 && capacityGB < 1024 {
			iops = customOf(&filestorepb.Instance_PerformanceConfig{Mode: &filestorepb.Instance_PerformanceConfig_IopsPerTb{
				IopsPerTb: &filestorepb.Instance_IOPSPerTB{MaxIopsPerTb: 4000}}}, capacityGB)
		}
	}
	if iops > 0 {
		return &filestorepb.Instance_PerformanceLimits{MaxIops: iops, MaxReadIops: iops,
			MaxWriteIops:          int64(math.Round(float64(iops) * 0.3)),
			MaxReadThroughputBps:  iops * 24576,
			MaxWriteThroughputBps: iops * 8192}
	}
	tib := float64(capacityGB) / 1024
	scaled := func(p perTiB) *filestorepb.Instance_PerformanceLimits {
		r := int64(math.Round(p.readIOPS * tib))
		return &filestorepb.Instance_PerformanceLimits{MaxIops: r, MaxReadIops: r,
			MaxWriteIops:          int64(math.Round(p.writeIOPS * tib)),
			MaxReadThroughputBps:  int64(math.Round(p.readMiB * tib * mib)),
			MaxWriteThroughputBps: int64(math.Round(p.writeMiB * tib * mib))}
	}
	fixed := func(r, w, rm, wm int64) *filestorepb.Instance_PerformanceLimits {
		return &filestorepb.Instance_PerformanceLimits{MaxIops: r, MaxReadIops: r, MaxWriteIops: w,
			MaxReadThroughputBps: rm * mib, MaxWriteThroughputBps: wm * mib}
	}
	switch tier {
	case filestorepb.Instance_BASIC_HDD, filestorepb.Instance_STANDARD:
		if capacityGB >= 10240 {
			return fixed(1000, 5000, 180, 120)
		}
		return fixed(600, 1000, 100, 100)
	case filestorepb.Instance_BASIC_SSD, filestorepb.Instance_PREMIUM:
		return fixed(60000, 25000, 1200, 350)
	case filestorepb.Instance_ENTERPRISE:
		return scaled(enterprisePerf)
	case filestorepb.Instance_REGIONAL:
		if capacityGB < 10240 {
			return scaled(enterprisePerf)
		}
		return scaled(zonalPerf)
	default:
		// ZONAL and HIGH_SCALE_SSD.
		return scaled(zonalPerf)
	}
}

// limitsOf is what the backend enforces of l.
func limitsOf(l *filestorepb.Instance_PerformanceLimits) Limits {
	return Limits{ReadIOPS: l.GetMaxReadIops(), WriteIOPS: l.GetMaxWriteIops(),
		ReadBps: l.GetMaxReadThroughputBps(), WriteBps: l.GetMaxWriteThroughputBps()}
}

// capacityOf is an instance's capacity: its file share's, or a
// multishare instance's own.
func (r *instRecord) capacityOf(in *filestorepb.Instance) int64 {
	if r.Multi != nil {
		return r.Multi.CapacityGB
	}
	if len(in.GetFileShares()) > 0 {
		return in.GetFileShares()[0].GetCapacityGb()
	}
	return 0
}

// setLimits sets in's performance_limits from its tier, capacity and
// performance_config.
func (r *instRecord) setLimits(in *filestorepb.Instance) {
	in.PerformanceLimits = PerformanceLimits(in.GetTier(), r.capacityOf(in), in.GetPerformanceConfig())
}
