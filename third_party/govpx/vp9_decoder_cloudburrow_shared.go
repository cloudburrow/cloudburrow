// CloudBurrow: this file is not part of upstream govpx. It collects, verbatim,
// the declarations the VP9 decoder shares with govpx's encoder source files
// (which are not vendored), so the decoder builds without the encoders.
// Each section names the upstream file and line range it was copied from
// (github.com/thesyncim/govpx@159dce5b931af4f7c600f157d64c2a813092ec57).

package govpx

import (
	"runtime"
	"sync"
	"sync/atomic"
	_ "unsafe" // for go:linkname

	"github.com/cloudburrow/cloudburrow/third_party/govpx/internal/vp9/common"
	vp9dec "github.com/cloudburrow/cloudburrow/third_party/govpx/internal/vp9/decoder"
	vp9rtp "github.com/cloudburrow/cloudburrow/third_party/govpx/internal/vp9/rtp"
)

// CloudBurrow: from vp8_encoder.go:18-23.

// runtimeProcYield issues the runtime's architecture-specific processor
// pause/yield instruction without handing the P back to the scheduler.
// Row-thread wave-front waits use it for short producer/consumer gaps.
//
//go:linkname runtimeProcYield runtime.procyield
func runtimeProcYield(cycles uint32)

// CloudBurrow: from vp8_encoder_row_worker.go:152-159.

const (
	// Helpers usually receive the next frame immediately after finishing
	// their row slice. Spin briefly before parking so steady realtime
	// encodes avoid runtime sudog churn on the worker start channels.
	rowWorkerIdleSpinBudget       = 65536
	rowWorkerIdleSchedulerBackoff = 4096
)

// CloudBurrow: from vp9_rtp.go:8-15 (the decoder uses VP9RTPMaxSpatialLayers).

const (
	// VP9RTPMaxReferenceIndices is the maximum number of reference-index
	// entries in a VP9 RTP payload descriptor or scalability-structure entry.
	VP9RTPMaxReferenceIndices = vp9rtp.MaxReferenceIndices
	// VP9RTPMaxSpatialLayers is the maximum number of spatial layers described
	// by a VP9 RTP scalability structure.
	VP9RTPMaxSpatialLayers = vp9rtp.MaxSpatialLayers
)

// CloudBurrow: from vp9_phase_stats_disabled.go (decoder-side no-op stubs;
// the govpx_phase_stats build tag variant is not vendored).

const vp9PhaseStatsEnabled = false

type vp9DecoderPhaseStatsOptions struct{}

func (d *VP9Decoder) vp9PhaseIncInterPredictPlane()   {}
func (d *VP9Decoder) vp9PhaseCountInterPredictor(int) {}

// CloudBurrow: from vp9_encoder_key_modes.go:1161-1173.

func vp9IntraPredictWidth4x4(bsize, planeBsize common.BlockSize,
	pd *vp9dec.MacroblockdPlane,
) int {
	w := int(common.Num4x4BlocksWideLookup[planeBsize])
	if pd == nil || bsize >= common.Block8x8 {
		return w
	}
	sub8W := max(2>>pd.SubsamplingX, 1)
	if sub8W > w {
		return sub8W
	}
	return w
}

// CloudBurrow: from vp9_encoder_tile_workers.go:165-256.

// vp9RowMTSync mirrors libvpx's VP9RowMTSync (vp9/encoder/vp9_ethread.h). It
// tracks the latest column index encoded for each SB row inside a tile column
// and exposes Read / Write primitives matching vp9_row_mt_sync_read /
// vp9_row_mt_sync_write. SyncRange (libvpx's sync_range) caps how far ahead a
// row can advance before publishing progress. Row jobs are active together for
// a short count-pass batch, so progress uses atomic handoff rather than parking
// and waking an OS thread for every SB.
type vp9RowMTSync struct {
	curCol    []atomic.Int32
	rows      int
	syncRange int
}

// vp9RowMTSyncDefaultRange mirrors libvpx's row_mt_sync->sync_range = 1 init in
// vp9_row_mt_sync_mem_alloc. A range of one means each completed SB column
// signals the row below; larger values reduce signalling frequency at the cost
// of more wavefront slack.
const vp9RowMTSyncDefaultRange = 1

func (s *vp9RowMTSync) reset(rows int) {
	if s == nil || rows <= 0 {
		return
	}
	if cap(s.curCol) < rows {
		s.curCol = make([]atomic.Int32, rows)
	} else {
		s.curCol = s.curCol[:rows]
	}
	for r := range s.curCol {
		s.curCol[r].Store(-1)
	}
	s.rows = rows
	if s.syncRange == 0 {
		s.syncRange = vp9RowMTSyncDefaultRange
	}
}

func (s *vp9RowMTSync) release() {
	if s == nil {
		return
	}
	s.curCol = s.curCol[:0]
	s.rows = 0
}

// read mirrors vp9_row_mt_sync_read. It blocks until the row above has reached
// column position c - syncRange + 1, ensuring the above and above-right SBs
// are encoded before the caller proceeds.
func (s *vp9RowMTSync) read(r, c int) {
	if s == nil || r <= 0 || r >= s.rows {
		return
	}
	nsync := s.syncRange
	if nsync <= 0 || (c&(nsync-1)) != 0 {
		return
	}
	target := int32(c - nsync + 1)
	for spins := 0; s.curCol[r-1].Load() < target; spins++ {
		runtimeProcYield(30)
		if spins >= rowWorkerIdleSchedulerBackoff &&
			spins%rowWorkerIdleSchedulerBackoff == 0 {
			runtime.Gosched()
		}
	}
}

// write mirrors vp9_row_mt_sync_write. It records the row's current column
// position and broadcasts to waiters when crossing a syncRange boundary or
// when the row finishes. cols is the SB column count for the tile.
func (s *vp9RowMTSync) write(r, c, cols int) {
	if s == nil || r < 0 || r >= s.rows {
		return
	}
	nsync := s.syncRange
	if nsync <= 0 {
		return
	}
	var cur int32
	sig := true
	if c < cols-1 {
		cur = int32(c)
		if c%nsync != nsync-1 {
			sig = false
		}
	} else {
		cur = int32(cols + nsync)
	}
	if !sig {
		return
	}
	s.curCol[r].Store(cur)
}

// CloudBurrow: from vp9_encoder_lf_workers.go:9-231 (the encoder-only
// applyVP9EncoderLoopFilterMT that follows is not vendored).

// vp9LfSync mirrors libvpx's VP9LfSync (vp9/common/vp9_thread_common.c): a
// per-SB-row column progress tracker that lets the row-interleaved loop
// filter workers stay one sync range behind the row above, since filtering a
// superblock's top horizontal edge writes pixels that the row above's
// vertical pass must have finished first.
type vp9LfSync struct {
	mu        []sync.Mutex
	cond      []*sync.Cond
	curSbCol  []int32
	tilesDone []int
	rows      int
	syncRange int
}

// vp9LfSyncRange mirrors get_sync_range (vp9_thread_common.c:249):
//
//	// nsync numbers are picked by testing. For example, for 4k
//	// video, using 4 gives best performance.
//	if (width < 640) return 1;
//	else if (width <= 1280) return 2;
//	else if (width <= 4096) return 4;
//	else return 8;
func vp9LfSyncRange(width int) int {
	switch {
	case width < 640:
		return 1
	case width <= 1280:
		return 2
	case width <= 4096:
		return 4
	default:
		return 8
	}
}

// reset re-arms the sync for a frame of sbRows rows, mirroring
// vp9_loop_filter_alloc's cur_sb_col allocation plus the memset to -1 in
// vp9_loop_filter_frame_mt (vp9_thread_common.c:214-216).
func (s *vp9LfSync) reset(sbRows, width int) {
	if s == nil || sbRows <= 0 {
		return
	}
	if cap(s.mu) < sbRows {
		s.mu = make([]sync.Mutex, sbRows)
		s.cond = make([]*sync.Cond, sbRows)
		s.curSbCol = make([]int32, sbRows)
		s.tilesDone = make([]int, sbRows)
		for r := range s.cond {
			s.cond[r] = sync.NewCond(&s.mu[r])
		}
	} else {
		s.mu = s.mu[:sbRows]
		s.cond = s.cond[:sbRows]
		s.curSbCol = s.curSbCol[:sbRows]
		s.tilesDone = s.tilesDone[:sbRows]
		for r := range s.cond {
			if s.cond[r] == nil {
				s.cond[r] = sync.NewCond(&s.mu[r])
			}
		}
	}
	for r := range s.curSbCol {
		s.curSbCol[r] = -1
		s.tilesDone[r] = 0
	}
	s.rows = sbRows
	s.syncRange = vp9LfSyncRange(width)
}

// tileRowDone mirrors lpf_map_write_check: the final tile reconstruction for
// an SB row releases exactly one LPF job for that row.
func (s *vp9LfSync) tileRowDone(row, tileCols int) bool {
	if s == nil || row < 0 || row >= s.rows || tileCols <= 0 {
		return false
	}
	mu := &s.mu[row]
	mu.Lock()
	s.tilesDone[row]++
	done := s.tilesDone[row] == tileCols
	mu.Unlock()
	return done
}

// read mirrors sync_read (vp9_thread_common.c:22): before filtering SB
// (r, c) the worker waits until row r-1 has advanced at least sync range
// columns past c.
func (s *vp9LfSync) read(r, c int) {
	if s == nil || r <= 0 || r >= s.rows {
		return
	}
	nsync := s.syncRange
	if nsync <= 0 || (c&(nsync-1)) != 0 {
		return
	}
	mu := &s.mu[r-1]
	mu.Lock()
	for int32(c) > s.curSbCol[r-1]-int32(nsync) {
		s.cond[r-1].Wait()
	}
	mu.Unlock()
}

// write mirrors sync_write (vp9_thread_common.c:42): record the row's
// progress and signal waiters when crossing a sync range boundary or when
// the row completes.
func (s *vp9LfSync) write(r, c, sbCols int) {
	if s == nil || r < 0 || r >= s.rows {
		return
	}
	nsync := s.syncRange
	if nsync <= 0 {
		return
	}
	var cur int32
	sig := true
	if c < sbCols-1 {
		cur = int32(c)
		if c%nsync != 0 {
			sig = false
		}
	} else {
		cur = int32(sbCols + nsync)
	}
	if !sig {
		return
	}
	mu := &s.mu[r]
	mu.Lock()
	s.curSbCol[r] = cur
	mu.Unlock()
	s.cond[r].Signal()
}

// markRowsDone releases any rows this worker still owns so sibling workers
// blocked in read never deadlock when a defensive filter-error bail fires.
func (s *vp9LfSync) markRowsDone(startSbRow, step, sbRows, sbCols int) {
	for r := startSbRow; r < sbRows; r += step {
		s.write(r, sbCols-1, sbCols)
	}
}

// vp9EncodeLfJob carries one worker's share of the row-interleaved loop
// filter walk, mirroring libvpx's LFWorkerData for thread_loop_filter_rows.
type vp9EncodeLfJob struct {
	d      VP9Decoder
	lfSync *vp9LfSync
	miRows int
	miCols int
	start  int
	step   int
	ok     bool
}

func runVP9EncodeLfJob(job *vp9EncodeLfJob) {
	if job == nil {
		return
	}
	job.ok = job.d.applyVP9LoopFilterRowsInterleaved(job.miRows, job.miCols,
		job.start, job.step, job.lfSync)
}

// applyVP9LoopFilterRowsInterleaved mirrors thread_loop_filter_rows
// (vp9_thread_common.c:74): the worker filters SB rows start, start+step,
// start+2*step, ... walking columns left to right under the lf sync
// wavefront. Masks must already be prepared for the whole frame.
func (d *VP9Decoder) applyVP9LoopFilterRowsInterleaved(miRows, miCols,
	start, step int, lfSync *vp9LfSync,
) bool {
	if step <= 0 {
		return false
	}
	sbRows := (miRows + common.MiBlockSize - 1) >> common.MiBlockSizeLog2
	sbCols := (miCols + common.MiBlockSize - 1) >> common.MiBlockSizeLog2
	for miRow := start * common.MiBlockSize; miRow < miRows; miRow += step * common.MiBlockSize {
		r := miRow >> common.MiBlockSizeLog2
		for miCol := 0; miCol < miCols; miCol += common.MiBlockSize {
			c := miCol >> common.MiBlockSizeLog2
			lfSync.read(r, c)
			lfm, ok := d.vp9LoopFilterMaskAt(miRow, miCol)
			if !ok || !d.vp9FilterLoopBlock(miRows, miRow, miCol, lfm) {
				// Defensive bail: release the rows this worker owns so
				// siblings blocked in read never deadlock, then report
				// the failure to the dispatcher.
				lfSync.write(r, sbCols-1, sbCols)
				lfSync.markRowsDone(r+step, step, sbRows, sbCols)
				return false
			}
			lfSync.write(r, c, sbCols)
		}
	}
	return true
}

// applyVP9LoopFilterRowMT mirrors vp9_loopfilter_job for one queued SB row.
// The previous reconstructed row is released by the following RECON job, and
// VP9LfSync preserves the left-to-right dependency between adjacent LPF rows.
func (d *VP9Decoder) applyVP9LoopFilterRowMT(miRows, miCols, miRow int,
	lfSync *vp9LfSync,
) bool {
	if lfSync == nil || miRow < 0 || miRow >= miRows {
		return false
	}
	endMiRow := min(miRow+common.MiBlockSize, miRows)
	if !d.prepareVP9LoopFilterMasksCached(miRows, miCols, miRow, endMiRow) {
		return false
	}
	sbRows := (miRows + common.MiBlockSize - 1) >> common.MiBlockSizeLog2
	sbCols := (miCols + common.MiBlockSize - 1) >> common.MiBlockSizeLog2
	r := miRow >> common.MiBlockSizeLog2
	for miCol := 0; miCol < miCols; miCol += common.MiBlockSize {
		c := miCol >> common.MiBlockSizeLog2
		lfSync.read(r, c)
		lfm, ok := d.vp9LoopFilterMaskAt(miRow, miCol)
		if !ok || !d.vp9FilterLoopBlock(miRows, miRow, miCol, lfm) {
			lfSync.write(r, sbCols-1, sbCols)
			lfSync.markRowsDone(r+1, 1, sbRows, sbCols)
			return false
		}
		lfSync.write(r, c, sbCols)
	}
	return true
}
