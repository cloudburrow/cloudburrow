package vp9hbd

// Constants and lookup tables of the VP9 bitstream specification.

const (
	keyFrame = 0

	intraFrame  = 0
	lastFrame   = 1
	goldenFrame = 2
	altrefFrame = 3
	noneFrame   = -1

	dcPred    = 0
	vPred     = 1
	hPred     = 2
	d45Pred   = 3
	d135Pred  = 4
	d117Pred  = 5
	d153Pred  = 6
	d207Pred  = 7
	d63Pred   = 8
	tmPred    = 9
	nearestMv = 10
	nearMv    = 11
	zeroMv    = 12
	newMv     = 13

	eightTap       = 0
	eightTapSmooth = 1
	eightTapSharp  = 2
	bilinear       = 3
	switchable     = 4

	tx4x4   = 0
	tx8x8   = 1
	tx16x16 = 2
	tx32x32 = 3

	only4x4      = 0
	allow32x32   = 3
	txModeSelect = 4

	singleReference     = 0
	compoundReference   = 1
	referenceModeSelect = 2

	dctDct   = 0
	adstDct  = 1
	dctAdst  = 2
	adstAdst = 3

	block4x4   = 0
	block8x8   = 3
	block16x16 = 6
	block64x64 = 12
	blockInv   = 13

	partNone  = 0
	partHorz  = 1
	partVert  = 2
	partSplit = 3

	segLvlAltQ     = 0
	segLvlAltL     = 1
	segLvlRefFrame = 2
	segLvlSkip     = 3

	csRGB = 7

	mvBorder      = 128
	interpExtend  = 4
	borderInPixel = 160
)

var (
	num4x4W       = [13]int{1, 1, 2, 2, 2, 4, 4, 4, 8, 8, 8, 16, 16}
	num4x4H       = [13]int{1, 2, 1, 2, 4, 2, 4, 8, 4, 8, 16, 8, 16}
	num8x8W       = [13]int{1, 1, 1, 1, 1, 2, 2, 2, 4, 4, 4, 8, 8}
	num8x8H       = [13]int{1, 1, 1, 1, 2, 1, 2, 4, 2, 4, 8, 4, 8}
	bWidthLog2    = [13]int{0, 0, 1, 1, 1, 2, 2, 2, 3, 3, 3, 4, 4}
	bHeightLog2   = [13]int{0, 1, 0, 1, 2, 1, 2, 3, 2, 3, 4, 3, 4}
	miWidthLog2   = [13]int{0, 0, 0, 0, 0, 1, 1, 1, 2, 2, 2, 3, 3}
	sizeGroup     = [13]int{0, 0, 0, 1, 1, 1, 2, 2, 2, 3, 3, 3, 3}
	maxTxSize     = [14]int{0, 0, 0, 1, 1, 1, 2, 2, 2, 3, 3, 3, 3, 0}
	txModeToBig   = [5]int{0, 1, 2, 3, 3}
	literalToType = [4]int{eightTapSmooth, eightTap, eightTapSharp, bilinear}
	subsizeLookup = [4][13]int{
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
		{blockInv, blockInv, blockInv, 2, blockInv, blockInv, 5, blockInv, blockInv, 8, blockInv, blockInv, 11},
		{blockInv, blockInv, blockInv, 1, blockInv, blockInv, 4, blockInv, blockInv, 7, blockInv, blockInv, 10},
		{blockInv, blockInv, blockInv, 0, blockInv, blockInv, 3, blockInv, blockInv, 6, blockInv, blockInv, 9},
	}
	mode2Txfm         = [14]int{dctDct, adstDct, dctAdst, dctDct, adstAdst, adstDct, dctAdst, dctAdst, adstDct, adstAdst, dctDct, dctDct, dctDct, dctDct}
	energyClass       = [11]uint8{0, 1, 2, 3, 3, 4, 4, 5, 5, 5, 5}
	segFeatureBits    = [4]int{8, 6, 2, 0}
	segFeatureSigned  = [4]bool{true, true, false, false}
	mode2Counter      = [14]int{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 0, 0, 3, 1}
	counterToContext  = [19]int{2, 3, 4, 1, 3, 9, 0, 9, 9, 5, 5, 9, 5, 9, 9, 9, 9, 9, 6}
	idxNColToSubblock = [4][2]int{{1, 2}, {1, 3}, {3, 2}, {3, 3}}
	extraBits         = [11][3]int{{0, 0, 0}, {0, 0, 1}, {0, 0, 2}, {0, 0, 3}, {0, 0, 4}, {1, 1, 5}, {2, 2, 7}, {3, 3, 11}, {4, 4, 19}, {5, 5, 35}, {6, 14, 67}}
	catProbs          = [7][]int{{}, {159}, {165, 145}, {173, 148, 140}, {176, 155, 140, 135}, {180, 157, 141, 134, 130}, {254, 254, 254, 252, 249, 243, 230, 196, 177, 153, 140, 133, 130, 129}}
	invMapTable       = [255]uint8{
		7, 20, 33, 46, 59, 72, 85, 98, 111, 124, 137, 150, 163, 176, 189,
		202, 215, 228, 241, 254, 1, 2, 3, 4, 5, 6, 8, 9, 10, 11,
		12, 13, 14, 15, 16, 17, 18, 19, 21, 22, 23, 24, 25, 26, 27,
		28, 29, 30, 31, 32, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43,
		44, 45, 47, 48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 60,
		61, 62, 63, 64, 65, 66, 67, 68, 69, 70, 71, 73, 74, 75, 76,
		77, 78, 79, 80, 81, 82, 83, 84, 86, 87, 88, 89, 90, 91, 92,
		93, 94, 95, 96, 97, 99, 100, 101, 102, 103, 104, 105, 106, 107, 108,
		109, 110, 112, 113, 114, 115, 116, 117, 118, 119, 120, 121, 122, 123, 125,
		126, 127, 128, 129, 130, 131, 132, 133, 134, 135, 136, 138, 139, 140, 141,
		142, 143, 144, 145, 146, 147, 148, 149, 151, 152, 153, 154, 155, 156, 157,
		158, 159, 160, 161, 162, 164, 165, 166, 167, 168, 169, 170, 171, 172, 173,
		174, 175, 177, 178, 179, 180, 181, 182, 183, 184, 185, 186, 187, 188, 190,
		191, 192, 193, 194, 195, 196, 197, 198, 199, 200, 201, 203, 204, 205, 206,
		207, 208, 209, 210, 211, 212, 213, 214, 216, 217, 218, 219, 220, 221, 222,
		223, 224, 225, 226, 227, 229, 230, 231, 232, 233, 234, 235, 236, 237, 238,
		239, 240, 242, 243, 244, 245, 246, 247, 248, 249, 250, 251, 252, 253, 253,
	}
)
