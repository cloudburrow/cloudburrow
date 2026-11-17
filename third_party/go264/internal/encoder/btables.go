package encoder

const (
	mbTypeBDirect = iota + 16
	mbTypeB16x16
	mbTypeB16x8
	mbTypeB8x16
	mbTypeB8x8
	mbTypeBSkip
)

const (
	predL0 = 1
	predL1 = 2
	predBi = predL0 | predL1
)

type bMBTypeInfo struct {
	kind   int
	pred   [2]uint8
	direct bool
}

var bMBTypes = [23]bMBTypeInfo{
	{kind: mbTypeBDirect, direct: true},
	{kind: mbTypeB16x16, pred: [2]uint8{predL0}},
	{kind: mbTypeB16x16, pred: [2]uint8{predL1}},
	{kind: mbTypeB16x16, pred: [2]uint8{predBi}},
	{kind: mbTypeB16x8, pred: [2]uint8{predL0, predL0}},
	{kind: mbTypeB8x16, pred: [2]uint8{predL0, predL0}},
	{kind: mbTypeB16x8, pred: [2]uint8{predL1, predL1}},
	{kind: mbTypeB8x16, pred: [2]uint8{predL1, predL1}},
	{kind: mbTypeB16x8, pred: [2]uint8{predL0, predL1}},
	{kind: mbTypeB8x16, pred: [2]uint8{predL0, predL1}},
	{kind: mbTypeB16x8, pred: [2]uint8{predL1, predL0}},
	{kind: mbTypeB8x16, pred: [2]uint8{predL1, predL0}},
	{kind: mbTypeB16x8, pred: [2]uint8{predL0, predBi}},
	{kind: mbTypeB8x16, pred: [2]uint8{predL0, predBi}},
	{kind: mbTypeB16x8, pred: [2]uint8{predL1, predBi}},
	{kind: mbTypeB8x16, pred: [2]uint8{predL1, predBi}},
	{kind: mbTypeB16x8, pred: [2]uint8{predBi, predL0}},
	{kind: mbTypeB8x16, pred: [2]uint8{predBi, predL0}},
	{kind: mbTypeB16x8, pred: [2]uint8{predBi, predL1}},
	{kind: mbTypeB8x16, pred: [2]uint8{predBi, predL1}},
	{kind: mbTypeB16x8, pred: [2]uint8{predBi, predBi}},
	{kind: mbTypeB8x16, pred: [2]uint8{predBi, predBi}},
	{kind: mbTypeB8x8},
}

type bSubTypeInfo struct {
	numParts int
	w, h     int
	pred     uint8
	direct   bool
}

var bSubTypes = [13]bSubTypeInfo{
	{numParts: 4, w: 4, h: 4, direct: true},
	{numParts: 1, w: 8, h: 8, pred: predL0},
	{numParts: 1, w: 8, h: 8, pred: predL1},
	{numParts: 1, w: 8, h: 8, pred: predBi},
	{numParts: 2, w: 8, h: 4, pred: predL0},
	{numParts: 2, w: 4, h: 8, pred: predL0},
	{numParts: 2, w: 8, h: 4, pred: predL1},
	{numParts: 2, w: 4, h: 8, pred: predL1},
	{numParts: 2, w: 8, h: 4, pred: predBi},
	{numParts: 2, w: 4, h: 8, pred: predBi},
	{numParts: 4, w: 4, h: 4, pred: predL0},
	{numParts: 4, w: 4, h: 4, pred: predL1},
	{numParts: 4, w: 4, h: 4, pred: predBi},
}

var bMBTypeIndex map[uint32]int

func bTypeKey(kind int, pred [2]uint8) uint32 {
	return uint32(kind)<<16 | uint32(pred[0])<<8 | uint32(pred[1])
}

func init() {
	bMBTypeIndex = make(map[uint32]int, len(bMBTypes))
	for i, info := range bMBTypes {
		if info.direct {
			continue
		}
		key := bTypeKey(info.kind, info.pred)
		if _, seen := bMBTypeIndex[key]; !seen {
			bMBTypeIndex[key] = i
		}
	}
}

func bMBTypeValue(kind int, pred [2]uint8) int {
	if v, ok := bMBTypeIndex[bTypeKey(kind, pred)]; ok {
		return v
	}
	return 0
}

func bPartitionsFor(kind int) []partition {
	switch kind {
	case mbTypeB16x16:
		return []partition{{0, 0, 16, 16}}
	case mbTypeB16x8:
		return []partition{{0, 0, 16, 8}, {0, 8, 16, 8}}
	case mbTypeB8x16:
		return []partition{{0, 0, 8, 16}, {8, 0, 8, 16}}
	}
	return nil
}
