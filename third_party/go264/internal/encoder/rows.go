package encoder

import (
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/parallel"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
)

const rowParallelMBs = 1024

type codedRow struct {
	slice    int
	mby      int
	after    int
	w        *bits.Writer
	coded    bool
	leading  uint32
	trailing uint32
	err      error
}

func (e *Encoder) rowsInParallel(hints *frameHints) bool {
	if e.pps.CABAC || hints.variesQP() || e.widthMBs*e.heightMBs < rowParallelMBs {
		return false
	}
	return parallel.Workers(e.heightMBs) > 1
}

func (e *Encoder) rowWriter(i int) *bits.Writer {
	for len(e.rowWriters) <= i {
		e.rowWriters = append(e.rowWriters, bits.NewWriterSize(e.widthMBs*64))
	}
	w := e.rowWriters[i]
	w.Reset()
	return w
}

func (e *Encoder) rowEncoder(lane int) *mbEncoder {
	for len(e.rowEncoders) <= lane {
		e.rowEncoders = append(e.rowEncoders, &mbEncoder{})
	}
	return e.rowEncoders[lane]
}

func (e *Encoder) encodeSliceRows(jobs []sliceJob, hints *frameHints) ([][]byte, error) {
	headers := make([]*syntax.SliceHeader, len(jobs))
	bySlice := make([][]int, len(jobs))
	var rows []codedRow
	for i, job := range jobs {
		headers[i] = e.sliceHeader(job)
	}
	for k := 0; ; k++ {
		added := false
		for i, job := range jobs {
			mby := job.firstMB/e.widthMBs + k
			if mby >= job.endMB/e.widthMBs {
				continue
			}
			after := -1
			if k > 0 {
				after = bySlice[i][k-1]
			}
			bySlice[i] = append(bySlice[i], len(rows))
			rows = append(rows, codedRow{slice: i, mby: mby, after: after, w: e.rowWriter(len(rows))})
			added = true
		}
		if !added {
			break
		}
	}
	lanes := len(rows)
	for lane := 0; lane < lanes; lane++ {
		e.rowEncoder(lane)
	}
	trail := func(r int) int { return rows[r].after }
	parallel.Rows(lanes, len(rows), e.widthMBs, 2, trail, func(lane, r int, wait, done func(int)) {
		row := &rows[r]
		job, hdr := jobs[row.slice], headers[row.slice]
		s := e.rowEncoders[lane]
		s.configure(e, row.w, hdr, job, hints)
		for mbx := 0; mbx < e.widthMBs; mbx++ {
			wait(mbx)
			before, pending := row.w.BitsWritten(), s.pendingSkipRun
			if err := s.encodeAt(row.mby*e.widthMBs+mbx, hdr, job.id+1); err != nil {
				row.err = err
				return
			}
			if !row.coded && row.w.BitsWritten() > before {
				row.coded, row.leading = true, pending
			}
			done(mbx)
		}
		row.trailing = s.pendingSkipRun
	})
	for i := range rows {
		if rows[i].err != nil {
			return nil, rows[i].err
		}
	}

	payloads := make([][]byte, len(jobs))
	errs := make([]error, len(jobs))
	parallel.For(len(jobs), 1, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			payloads[i], errs[i] = e.joinRows(headers[i], jobs[i], rows, bySlice[i])
		}
	})
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return payloads, nil
}

func (e *Encoder) joinRows(hdr *syntax.SliceHeader, job sliceJob, rows []codedRow, order []int) ([]byte, error) {
	w := bits.NewWriterSize(e.cfg.Width*e.cfg.Height/2/job.count + 64)
	if err := syntax.WriteSliceHeader(w, hdr, e.sps, e.pps); err != nil {
		return nil, err
	}
	runs := hdr.SliceType.IsP() || hdr.SliceType.IsB()
	carry := uint32(0)
	for _, i := range order {
		r := &rows[i]
		switch {
		case !runs:
			w.AppendBits(r.w, 0)
		case !r.coded:
			carry += r.trailing
		default:
			w.WriteUE(carry + r.leading)
			w.AppendBits(r.w, bitsForUE(r.leading))
			carry = r.trailing
		}
	}
	if runs && carry > 0 {
		w.WriteUE(carry)
	}
	w.WriteRBSPTrailingBits()
	if err := w.Err(); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}
