package vp9hbd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFeatures(t *testing.T) {
	dir := os.Getenv("VP9HBD_DEV")
	if dir == "" {
		t.Skip()
	}
	ms, _ := filepath.Glob(filepath.Join(dir, "*.ivf"))
	for _, m := range ms {
		d := NewDecoder()
		f := map[string]int{}
		for _, s := range readIVF(t, m) {
			for _, fr := range splitSuperframe(s) {
				if _, err := d.decodeFrame(fr); err != nil {
					t.Fatal(err)
				}
				h := &d.h
				if h.showExistingFrame { f["showex"]++; continue }
				if !h.showFrame { f["hidden"]++ }
				if h.intraOnly { f["intraonly"]++ }
				if d.seg.enabled { f["seg"]++ }
				if d.seg.temporalUpdate { f["segtemporal"]++ }
				if h.tileColsLog2 > 0 { f["tiles"]++ }
				if h.interpFilter == switchable { f["switchable"]++ }
				if h.lossless { f["lossless"]++ }
				if h.errorResilient { f["errres"]++ }
				if h.referenceMode != singleReference && !h.frameIsIntra { f["compmode"]++ }
				if h.txMode == txModeSelect { f["txselect"]++ }
				if d.usePrevFrameMvs { f["prevmvs"]++ }
				if h.allowHighPrecisionMv { f["hp"]++ }
				for _, mi := range d.mis {
					if mi.refFrame[1] > 0 { f["compblk"]++ }
					if mi.txSize == 3 { f["tx32"]++ }
					if mi.miSize < 3 && mi.refFrame[0] > 0 { f["sub8inter"]++ }
					if mi.miSize < 3 && mi.refFrame[0] == 0 { f["sub8intra"]++ }
					if mi.refFrame[0] == 0 && !h.frameIsIntra { f["intrainter"]++ }
					if mi.segmentID > 0 { f["segid"]++ }
				}
			}
		}
		t.Logf("%s %v", filepath.Base(m), f)
	}
}
