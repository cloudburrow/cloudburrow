package vp9hbd

// The 1-D inverse transforms below are govpx's ports of libvpx's
// vpx_dsp/inv_txfm.c (BSD 3-Clause, ../internal/vp9/dsp), widened from int16 to
// int32 as libvpx's high-bitdepth build (tran_low_t = int32_t) computes them,
// for every bit depth.

const (
	dctConstBits     = 14
	unitQuantShift   = 2
	dctConstRounding = 1 << (dctConstBits - 1)
)
const (
	cospi1_64  = 16364
	cospi2_64  = 16305
	cospi3_64  = 16207
	cospi4_64  = 16069
	cospi5_64  = 15893
	cospi6_64  = 15679
	cospi7_64  = 15426
	cospi8_64  = 15137
	cospi9_64  = 14811
	cospi10_64 = 14449
	cospi11_64 = 14053
	cospi12_64 = 13623
	cospi13_64 = 13160
	cospi14_64 = 12665
	cospi15_64 = 12140
	cospi16_64 = 11585
	cospi17_64 = 11003
	cospi18_64 = 10394
	cospi19_64 = 9760
	cospi20_64 = 9102
	cospi21_64 = 8423
	cospi22_64 = 7723
	cospi23_64 = 7005
	cospi24_64 = 6270
	cospi25_64 = 5520
	cospi26_64 = 4756
	cospi27_64 = 3981
	cospi28_64 = 3196
	cospi29_64 = 2404
	cospi30_64 = 1606
	cospi31_64 = 804

	sinpi1_9 = 5283
	sinpi2_9 = 9929
	sinpi3_9 = 13377
	sinpi4_9 = 15212
)

func idct4(input, output []int32) {
	var step [4]int32
	in0 := int64(input[0])
	in1 := int64(input[1])
	in2 := int64(input[2])
	in3 := int64(input[3])

	// stage 1
	temp1 := (in0 + in2) * cospi16_64
	temp2 := (in0 - in2) * cospi16_64
	step[0] = int32(dctConstRoundShift(temp1))
	step[1] = int32(dctConstRoundShift(temp2))
	temp1 = in1*cospi24_64 - in3*cospi8_64
	temp2 = in1*cospi8_64 + in3*cospi24_64
	step[2] = int32(dctConstRoundShift(temp1))
	step[3] = int32(dctConstRoundShift(temp2))

	// stage 2
	output[0] = int32(step[0] + step[3])
	output[1] = int32(step[1] + step[2])
	output[2] = int32(step[1] - step[2])
	output[3] = int32(step[0] - step[3])
}

func idct8(input, output []int32) {
	var step1, step2 [8]int32
	in0 := int64(input[0])
	in1 := int64(input[1])
	in2 := int64(input[2])
	in3 := int64(input[3])
	in4 := int64(input[4])
	in5 := int64(input[5])
	in6 := int64(input[6])
	in7 := int64(input[7])

	// stage 1
	step1[0] = int32(in0)
	step1[2] = int32(in4)
	step1[1] = int32(in2)
	step1[3] = int32(in6)
	temp1 := in1*cospi28_64 - in7*cospi4_64
	temp2 := in1*cospi4_64 + in7*cospi28_64
	step1[4] = int32(dctConstRoundShift(temp1))
	step1[7] = int32(dctConstRoundShift(temp2))
	temp1 = in5*cospi12_64 - in3*cospi20_64
	temp2 = in5*cospi20_64 + in3*cospi12_64
	step1[5] = int32(dctConstRoundShift(temp1))
	step1[6] = int32(dctConstRoundShift(temp2))

	// stage 2
	temp1 = (int64(step1[0]) + int64(step1[2])) * cospi16_64
	temp2 = (int64(step1[0]) - int64(step1[2])) * cospi16_64
	step2[0] = int32(dctConstRoundShift(temp1))
	step2[1] = int32(dctConstRoundShift(temp2))
	temp1 = int64(step1[1])*cospi24_64 - int64(step1[3])*cospi8_64
	temp2 = int64(step1[1])*cospi8_64 + int64(step1[3])*cospi24_64
	step2[2] = int32(dctConstRoundShift(temp1))
	step2[3] = int32(dctConstRoundShift(temp2))
	step2[4] = int32(int64(step1[4]) + int64(step1[5]))
	step2[5] = int32(int64(step1[4]) - int64(step1[5]))
	step2[6] = int32(-int64(step1[6]) + int64(step1[7]))
	step2[7] = int32(int64(step1[6]) + int64(step1[7]))

	// stage 3
	step1[0] = int32(int64(step2[0]) + int64(step2[3]))
	step1[1] = int32(int64(step2[1]) + int64(step2[2]))
	step1[2] = int32(int64(step2[1]) - int64(step2[2]))
	step1[3] = int32(int64(step2[0]) - int64(step2[3]))
	step1[4] = step2[4]
	temp1 = (int64(step2[6]) - int64(step2[5])) * cospi16_64
	temp2 = (int64(step2[5]) + int64(step2[6])) * cospi16_64
	step1[5] = int32(dctConstRoundShift(temp1))
	step1[6] = int32(dctConstRoundShift(temp2))
	step1[7] = step2[7]

	// stage 4
	output[0] = int32(int64(step1[0]) + int64(step1[7]))
	output[1] = int32(int64(step1[1]) + int64(step1[6]))
	output[2] = int32(int64(step1[2]) + int64(step1[5]))
	output[3] = int32(int64(step1[3]) + int64(step1[4]))
	output[4] = int32(int64(step1[3]) - int64(step1[4]))
	output[5] = int32(int64(step1[2]) - int64(step1[5]))
	output[6] = int32(int64(step1[1]) - int64(step1[6]))
	output[7] = int32(int64(step1[0]) - int64(step1[7]))
}

func idct16(input, output []int32) {
	var step1, step2 [16]int32

	// stage 1 — bit-reverse permutation. Inputs are read as
	//   step1[k] = input[bitrev(k, 16) / 2]
	// matching the literal `input[0/2], input[16/2], ...` form in libvpx.
	step1[0] = input[0]
	step1[1] = input[8]
	step1[2] = input[4]
	step1[3] = input[12]
	step1[4] = input[2]
	step1[5] = input[10]
	step1[6] = input[6]
	step1[7] = input[14]
	step1[8] = input[1]
	step1[9] = input[9]
	step1[10] = input[5]
	step1[11] = input[13]
	step1[12] = input[3]
	step1[13] = input[11]
	step1[14] = input[7]
	step1[15] = input[15]

	// stage 2
	step2[0] = step1[0]
	step2[1] = step1[1]
	step2[2] = step1[2]
	step2[3] = step1[3]
	step2[4] = step1[4]
	step2[5] = step1[5]
	step2[6] = step1[6]
	step2[7] = step1[7]

	temp1 := int64(step1[8])*cospi30_64 - int64(step1[15])*cospi2_64
	temp2 := int64(step1[8])*cospi2_64 + int64(step1[15])*cospi30_64
	step2[8] = int32(dctConstRoundShift(temp1))
	step2[15] = int32(dctConstRoundShift(temp2))

	temp1 = int64(step1[9])*cospi14_64 - int64(step1[14])*cospi18_64
	temp2 = int64(step1[9])*cospi18_64 + int64(step1[14])*cospi14_64
	step2[9] = int32(dctConstRoundShift(temp1))
	step2[14] = int32(dctConstRoundShift(temp2))

	temp1 = int64(step1[10])*cospi22_64 - int64(step1[13])*cospi10_64
	temp2 = int64(step1[10])*cospi10_64 + int64(step1[13])*cospi22_64
	step2[10] = int32(dctConstRoundShift(temp1))
	step2[13] = int32(dctConstRoundShift(temp2))

	temp1 = int64(step1[11])*cospi6_64 - int64(step1[12])*cospi26_64
	temp2 = int64(step1[11])*cospi26_64 + int64(step1[12])*cospi6_64
	step2[11] = int32(dctConstRoundShift(temp1))
	step2[12] = int32(dctConstRoundShift(temp2))

	// stage 3
	step1[0] = step2[0]
	step1[1] = step2[1]
	step1[2] = step2[2]
	step1[3] = step2[3]

	temp1 = int64(step2[4])*cospi28_64 - int64(step2[7])*cospi4_64
	temp2 = int64(step2[4])*cospi4_64 + int64(step2[7])*cospi28_64
	step1[4] = int32(dctConstRoundShift(temp1))
	step1[7] = int32(dctConstRoundShift(temp2))
	temp1 = int64(step2[5])*cospi12_64 - int64(step2[6])*cospi20_64
	temp2 = int64(step2[5])*cospi20_64 + int64(step2[6])*cospi12_64
	step1[5] = int32(dctConstRoundShift(temp1))
	step1[6] = int32(dctConstRoundShift(temp2))

	step1[8] = int32(int64(step2[8]) + int64(step2[9]))
	step1[9] = int32(int64(step2[8]) - int64(step2[9]))
	step1[10] = int32(-int64(step2[10]) + int64(step2[11]))
	step1[11] = int32(int64(step2[10]) + int64(step2[11]))
	step1[12] = int32(int64(step2[12]) + int64(step2[13]))
	step1[13] = int32(int64(step2[12]) - int64(step2[13]))
	step1[14] = int32(-int64(step2[14]) + int64(step2[15]))
	step1[15] = int32(int64(step2[14]) + int64(step2[15]))

	// stage 4
	temp1 = (int64(step1[0]) + int64(step1[1])) * cospi16_64
	temp2 = (int64(step1[0]) - int64(step1[1])) * cospi16_64
	step2[0] = int32(dctConstRoundShift(temp1))
	step2[1] = int32(dctConstRoundShift(temp2))
	temp1 = int64(step1[2])*cospi24_64 - int64(step1[3])*cospi8_64
	temp2 = int64(step1[2])*cospi8_64 + int64(step1[3])*cospi24_64
	step2[2] = int32(dctConstRoundShift(temp1))
	step2[3] = int32(dctConstRoundShift(temp2))
	step2[4] = int32(int64(step1[4]) + int64(step1[5]))
	step2[5] = int32(int64(step1[4]) - int64(step1[5]))
	step2[6] = int32(-int64(step1[6]) + int64(step1[7]))
	step2[7] = int32(int64(step1[6]) + int64(step1[7]))

	step2[8] = step1[8]
	step2[15] = step1[15]
	temp1 = -int64(step1[9])*cospi8_64 + int64(step1[14])*cospi24_64
	temp2 = int64(step1[9])*cospi24_64 + int64(step1[14])*cospi8_64
	step2[9] = int32(dctConstRoundShift(temp1))
	step2[14] = int32(dctConstRoundShift(temp2))
	temp1 = -int64(step1[10])*cospi24_64 - int64(step1[13])*cospi8_64
	temp2 = -int64(step1[10])*cospi8_64 + int64(step1[13])*cospi24_64
	step2[10] = int32(dctConstRoundShift(temp1))
	step2[13] = int32(dctConstRoundShift(temp2))
	step2[11] = step1[11]
	step2[12] = step1[12]

	// stage 5
	step1[0] = int32(int64(step2[0]) + int64(step2[3]))
	step1[1] = int32(int64(step2[1]) + int64(step2[2]))
	step1[2] = int32(int64(step2[1]) - int64(step2[2]))
	step1[3] = int32(int64(step2[0]) - int64(step2[3]))
	step1[4] = step2[4]
	temp1 = (int64(step2[6]) - int64(step2[5])) * cospi16_64
	temp2 = (int64(step2[5]) + int64(step2[6])) * cospi16_64
	step1[5] = int32(dctConstRoundShift(temp1))
	step1[6] = int32(dctConstRoundShift(temp2))
	step1[7] = step2[7]

	step1[8] = int32(int64(step2[8]) + int64(step2[11]))
	step1[9] = int32(int64(step2[9]) + int64(step2[10]))
	step1[10] = int32(int64(step2[9]) - int64(step2[10]))
	step1[11] = int32(int64(step2[8]) - int64(step2[11]))
	step1[12] = int32(-int64(step2[12]) + int64(step2[15]))
	step1[13] = int32(-int64(step2[13]) + int64(step2[14]))
	step1[14] = int32(int64(step2[13]) + int64(step2[14]))
	step1[15] = int32(int64(step2[12]) + int64(step2[15]))

	// stage 6
	step2[0] = int32(int64(step1[0]) + int64(step1[7]))
	step2[1] = int32(int64(step1[1]) + int64(step1[6]))
	step2[2] = int32(int64(step1[2]) + int64(step1[5]))
	step2[3] = int32(int64(step1[3]) + int64(step1[4]))
	step2[4] = int32(int64(step1[3]) - int64(step1[4]))
	step2[5] = int32(int64(step1[2]) - int64(step1[5]))
	step2[6] = int32(int64(step1[1]) - int64(step1[6]))
	step2[7] = int32(int64(step1[0]) - int64(step1[7]))
	step2[8] = step1[8]
	step2[9] = step1[9]
	temp1 = (-int64(step1[10]) + int64(step1[13])) * cospi16_64
	temp2 = (int64(step1[10]) + int64(step1[13])) * cospi16_64
	step2[10] = int32(dctConstRoundShift(temp1))
	step2[13] = int32(dctConstRoundShift(temp2))
	temp1 = (-int64(step1[11]) + int64(step1[12])) * cospi16_64
	temp2 = (int64(step1[11]) + int64(step1[12])) * cospi16_64
	step2[11] = int32(dctConstRoundShift(temp1))
	step2[12] = int32(dctConstRoundShift(temp2))
	step2[14] = step1[14]
	step2[15] = step1[15]

	// stage 7
	output[0] = int32(int64(step2[0]) + int64(step2[15]))
	output[1] = int32(int64(step2[1]) + int64(step2[14]))
	output[2] = int32(int64(step2[2]) + int64(step2[13]))
	output[3] = int32(int64(step2[3]) + int64(step2[12]))
	output[4] = int32(int64(step2[4]) + int64(step2[11]))
	output[5] = int32(int64(step2[5]) + int64(step2[10]))
	output[6] = int32(int64(step2[6]) + int64(step2[9]))
	output[7] = int32(int64(step2[7]) + int64(step2[8]))
	output[8] = int32(int64(step2[7]) - int64(step2[8]))
	output[9] = int32(int64(step2[6]) - int64(step2[9]))
	output[10] = int32(int64(step2[5]) - int64(step2[10]))
	output[11] = int32(int64(step2[4]) - int64(step2[11]))
	output[12] = int32(int64(step2[3]) - int64(step2[12]))
	output[13] = int32(int64(step2[2]) - int64(step2[13]))
	output[14] = int32(int64(step2[1]) - int64(step2[14]))
	output[15] = int32(int64(step2[0]) - int64(step2[15]))
}

func idct32(input, output []int32) {
	var step1, step2 [32]int32

	// stage 1
	step1[0] = input[0]
	step1[1] = input[16]
	step1[2] = input[8]
	step1[3] = input[24]
	step1[4] = input[4]
	step1[5] = input[20]
	step1[6] = input[12]
	step1[7] = input[28]
	step1[8] = input[2]
	step1[9] = input[18]
	step1[10] = input[10]
	step1[11] = input[26]
	step1[12] = input[6]
	step1[13] = input[22]
	step1[14] = input[14]
	step1[15] = input[30]

	temp1 := int64(input[1])*cospi31_64 - int64(input[31])*cospi1_64
	temp2 := int64(input[1])*cospi1_64 + int64(input[31])*cospi31_64
	step1[16] = int32(dctConstRoundShift(temp1))
	step1[31] = int32(dctConstRoundShift(temp2))

	temp1 = int64(input[17])*cospi15_64 - int64(input[15])*cospi17_64
	temp2 = int64(input[17])*cospi17_64 + int64(input[15])*cospi15_64
	step1[17] = int32(dctConstRoundShift(temp1))
	step1[30] = int32(dctConstRoundShift(temp2))

	temp1 = int64(input[9])*cospi23_64 - int64(input[23])*cospi9_64
	temp2 = int64(input[9])*cospi9_64 + int64(input[23])*cospi23_64
	step1[18] = int32(dctConstRoundShift(temp1))
	step1[29] = int32(dctConstRoundShift(temp2))

	temp1 = int64(input[25])*cospi7_64 - int64(input[7])*cospi25_64
	temp2 = int64(input[25])*cospi25_64 + int64(input[7])*cospi7_64
	step1[19] = int32(dctConstRoundShift(temp1))
	step1[28] = int32(dctConstRoundShift(temp2))

	temp1 = int64(input[5])*cospi27_64 - int64(input[27])*cospi5_64
	temp2 = int64(input[5])*cospi5_64 + int64(input[27])*cospi27_64
	step1[20] = int32(dctConstRoundShift(temp1))
	step1[27] = int32(dctConstRoundShift(temp2))

	temp1 = int64(input[21])*cospi11_64 - int64(input[11])*cospi21_64
	temp2 = int64(input[21])*cospi21_64 + int64(input[11])*cospi11_64
	step1[21] = int32(dctConstRoundShift(temp1))
	step1[26] = int32(dctConstRoundShift(temp2))

	temp1 = int64(input[13])*cospi19_64 - int64(input[19])*cospi13_64
	temp2 = int64(input[13])*cospi13_64 + int64(input[19])*cospi19_64
	step1[22] = int32(dctConstRoundShift(temp1))
	step1[25] = int32(dctConstRoundShift(temp2))

	temp1 = int64(input[29])*cospi3_64 - int64(input[3])*cospi29_64
	temp2 = int64(input[29])*cospi29_64 + int64(input[3])*cospi3_64
	step1[23] = int32(dctConstRoundShift(temp1))
	step1[24] = int32(dctConstRoundShift(temp2))

	// stage 2
	step2[0] = step1[0]
	step2[1] = step1[1]
	step2[2] = step1[2]
	step2[3] = step1[3]
	step2[4] = step1[4]
	step2[5] = step1[5]
	step2[6] = step1[6]
	step2[7] = step1[7]

	temp1 = int64(step1[8])*cospi30_64 - int64(step1[15])*cospi2_64
	temp2 = int64(step1[8])*cospi2_64 + int64(step1[15])*cospi30_64
	step2[8] = int32(dctConstRoundShift(temp1))
	step2[15] = int32(dctConstRoundShift(temp2))

	temp1 = int64(step1[9])*cospi14_64 - int64(step1[14])*cospi18_64
	temp2 = int64(step1[9])*cospi18_64 + int64(step1[14])*cospi14_64
	step2[9] = int32(dctConstRoundShift(temp1))
	step2[14] = int32(dctConstRoundShift(temp2))

	temp1 = int64(step1[10])*cospi22_64 - int64(step1[13])*cospi10_64
	temp2 = int64(step1[10])*cospi10_64 + int64(step1[13])*cospi22_64
	step2[10] = int32(dctConstRoundShift(temp1))
	step2[13] = int32(dctConstRoundShift(temp2))

	temp1 = int64(step1[11])*cospi6_64 - int64(step1[12])*cospi26_64
	temp2 = int64(step1[11])*cospi26_64 + int64(step1[12])*cospi6_64
	step2[11] = int32(dctConstRoundShift(temp1))
	step2[12] = int32(dctConstRoundShift(temp2))

	step2[16] = int32(int64(step1[16]) + int64(step1[17]))
	step2[17] = int32(int64(step1[16]) - int64(step1[17]))
	step2[18] = int32(-int64(step1[18]) + int64(step1[19]))
	step2[19] = int32(int64(step1[18]) + int64(step1[19]))
	step2[20] = int32(int64(step1[20]) + int64(step1[21]))
	step2[21] = int32(int64(step1[20]) - int64(step1[21]))
	step2[22] = int32(-int64(step1[22]) + int64(step1[23]))
	step2[23] = int32(int64(step1[22]) + int64(step1[23]))
	step2[24] = int32(int64(step1[24]) + int64(step1[25]))
	step2[25] = int32(int64(step1[24]) - int64(step1[25]))
	step2[26] = int32(-int64(step1[26]) + int64(step1[27]))
	step2[27] = int32(int64(step1[26]) + int64(step1[27]))
	step2[28] = int32(int64(step1[28]) + int64(step1[29]))
	step2[29] = int32(int64(step1[28]) - int64(step1[29]))
	step2[30] = int32(-int64(step1[30]) + int64(step1[31]))
	step2[31] = int32(int64(step1[30]) + int64(step1[31]))

	// stage 3
	step1[0] = step2[0]
	step1[1] = step2[1]
	step1[2] = step2[2]
	step1[3] = step2[3]

	temp1 = int64(step2[4])*cospi28_64 - int64(step2[7])*cospi4_64
	temp2 = int64(step2[4])*cospi4_64 + int64(step2[7])*cospi28_64
	step1[4] = int32(dctConstRoundShift(temp1))
	step1[7] = int32(dctConstRoundShift(temp2))
	temp1 = int64(step2[5])*cospi12_64 - int64(step2[6])*cospi20_64
	temp2 = int64(step2[5])*cospi20_64 + int64(step2[6])*cospi12_64
	step1[5] = int32(dctConstRoundShift(temp1))
	step1[6] = int32(dctConstRoundShift(temp2))

	step1[8] = int32(int64(step2[8]) + int64(step2[9]))
	step1[9] = int32(int64(step2[8]) - int64(step2[9]))
	step1[10] = int32(-int64(step2[10]) + int64(step2[11]))
	step1[11] = int32(int64(step2[10]) + int64(step2[11]))
	step1[12] = int32(int64(step2[12]) + int64(step2[13]))
	step1[13] = int32(int64(step2[12]) - int64(step2[13]))
	step1[14] = int32(-int64(step2[14]) + int64(step2[15]))
	step1[15] = int32(int64(step2[14]) + int64(step2[15]))

	step1[16] = step2[16]
	step1[31] = step2[31]
	temp1 = -int64(step2[17])*cospi4_64 + int64(step2[30])*cospi28_64
	temp2 = int64(step2[17])*cospi28_64 + int64(step2[30])*cospi4_64
	step1[17] = int32(dctConstRoundShift(temp1))
	step1[30] = int32(dctConstRoundShift(temp2))
	temp1 = -int64(step2[18])*cospi28_64 - int64(step2[29])*cospi4_64
	temp2 = -int64(step2[18])*cospi4_64 + int64(step2[29])*cospi28_64
	step1[18] = int32(dctConstRoundShift(temp1))
	step1[29] = int32(dctConstRoundShift(temp2))
	step1[19] = step2[19]
	step1[20] = step2[20]
	temp1 = -int64(step2[21])*cospi20_64 + int64(step2[26])*cospi12_64
	temp2 = int64(step2[21])*cospi12_64 + int64(step2[26])*cospi20_64
	step1[21] = int32(dctConstRoundShift(temp1))
	step1[26] = int32(dctConstRoundShift(temp2))
	temp1 = -int64(step2[22])*cospi12_64 - int64(step2[25])*cospi20_64
	temp2 = -int64(step2[22])*cospi20_64 + int64(step2[25])*cospi12_64
	step1[22] = int32(dctConstRoundShift(temp1))
	step1[25] = int32(dctConstRoundShift(temp2))
	step1[23] = step2[23]
	step1[24] = step2[24]
	step1[27] = step2[27]
	step1[28] = step2[28]

	// stage 4
	temp1 = (int64(step1[0]) + int64(step1[1])) * cospi16_64
	temp2 = (int64(step1[0]) - int64(step1[1])) * cospi16_64
	step2[0] = int32(dctConstRoundShift(temp1))
	step2[1] = int32(dctConstRoundShift(temp2))
	temp1 = int64(step1[2])*cospi24_64 - int64(step1[3])*cospi8_64
	temp2 = int64(step1[2])*cospi8_64 + int64(step1[3])*cospi24_64
	step2[2] = int32(dctConstRoundShift(temp1))
	step2[3] = int32(dctConstRoundShift(temp2))
	step2[4] = int32(int64(step1[4]) + int64(step1[5]))
	step2[5] = int32(int64(step1[4]) - int64(step1[5]))
	step2[6] = int32(-int64(step1[6]) + int64(step1[7]))
	step2[7] = int32(int64(step1[6]) + int64(step1[7]))

	step2[8] = step1[8]
	step2[15] = step1[15]
	temp1 = -int64(step1[9])*cospi8_64 + int64(step1[14])*cospi24_64
	temp2 = int64(step1[9])*cospi24_64 + int64(step1[14])*cospi8_64
	step2[9] = int32(dctConstRoundShift(temp1))
	step2[14] = int32(dctConstRoundShift(temp2))
	temp1 = -int64(step1[10])*cospi24_64 - int64(step1[13])*cospi8_64
	temp2 = -int64(step1[10])*cospi8_64 + int64(step1[13])*cospi24_64
	step2[10] = int32(dctConstRoundShift(temp1))
	step2[13] = int32(dctConstRoundShift(temp2))
	step2[11] = step1[11]
	step2[12] = step1[12]

	step2[16] = int32(int64(step1[16]) + int64(step1[19]))
	step2[17] = int32(int64(step1[17]) + int64(step1[18]))
	step2[18] = int32(int64(step1[17]) - int64(step1[18]))
	step2[19] = int32(int64(step1[16]) - int64(step1[19]))
	step2[20] = int32(-int64(step1[20]) + int64(step1[23]))
	step2[21] = int32(-int64(step1[21]) + int64(step1[22]))
	step2[22] = int32(int64(step1[21]) + int64(step1[22]))
	step2[23] = int32(int64(step1[20]) + int64(step1[23]))

	step2[24] = int32(int64(step1[24]) + int64(step1[27]))
	step2[25] = int32(int64(step1[25]) + int64(step1[26]))
	step2[26] = int32(int64(step1[25]) - int64(step1[26]))
	step2[27] = int32(int64(step1[24]) - int64(step1[27]))
	step2[28] = int32(-int64(step1[28]) + int64(step1[31]))
	step2[29] = int32(-int64(step1[29]) + int64(step1[30]))
	step2[30] = int32(int64(step1[29]) + int64(step1[30]))
	step2[31] = int32(int64(step1[28]) + int64(step1[31]))

	// stage 5
	step1[0] = int32(int64(step2[0]) + int64(step2[3]))
	step1[1] = int32(int64(step2[1]) + int64(step2[2]))
	step1[2] = int32(int64(step2[1]) - int64(step2[2]))
	step1[3] = int32(int64(step2[0]) - int64(step2[3]))
	step1[4] = step2[4]
	temp1 = (int64(step2[6]) - int64(step2[5])) * cospi16_64
	temp2 = (int64(step2[5]) + int64(step2[6])) * cospi16_64
	step1[5] = int32(dctConstRoundShift(temp1))
	step1[6] = int32(dctConstRoundShift(temp2))
	step1[7] = step2[7]

	step1[8] = int32(int64(step2[8]) + int64(step2[11]))
	step1[9] = int32(int64(step2[9]) + int64(step2[10]))
	step1[10] = int32(int64(step2[9]) - int64(step2[10]))
	step1[11] = int32(int64(step2[8]) - int64(step2[11]))
	step1[12] = int32(-int64(step2[12]) + int64(step2[15]))
	step1[13] = int32(-int64(step2[13]) + int64(step2[14]))
	step1[14] = int32(int64(step2[13]) + int64(step2[14]))
	step1[15] = int32(int64(step2[12]) + int64(step2[15]))

	step1[16] = step2[16]
	step1[17] = step2[17]
	temp1 = -int64(step2[18])*cospi8_64 + int64(step2[29])*cospi24_64
	temp2 = int64(step2[18])*cospi24_64 + int64(step2[29])*cospi8_64
	step1[18] = int32(dctConstRoundShift(temp1))
	step1[29] = int32(dctConstRoundShift(temp2))
	temp1 = -int64(step2[19])*cospi8_64 + int64(step2[28])*cospi24_64
	temp2 = int64(step2[19])*cospi24_64 + int64(step2[28])*cospi8_64
	step1[19] = int32(dctConstRoundShift(temp1))
	step1[28] = int32(dctConstRoundShift(temp2))
	temp1 = -int64(step2[20])*cospi24_64 - int64(step2[27])*cospi8_64
	temp2 = -int64(step2[20])*cospi8_64 + int64(step2[27])*cospi24_64
	step1[20] = int32(dctConstRoundShift(temp1))
	step1[27] = int32(dctConstRoundShift(temp2))
	temp1 = -int64(step2[21])*cospi24_64 - int64(step2[26])*cospi8_64
	temp2 = -int64(step2[21])*cospi8_64 + int64(step2[26])*cospi24_64
	step1[21] = int32(dctConstRoundShift(temp1))
	step1[26] = int32(dctConstRoundShift(temp2))
	step1[22] = step2[22]
	step1[23] = step2[23]
	step1[24] = step2[24]
	step1[25] = step2[25]
	step1[30] = step2[30]
	step1[31] = step2[31]

	// stage 6
	step2[0] = int32(int64(step1[0]) + int64(step1[7]))
	step2[1] = int32(int64(step1[1]) + int64(step1[6]))
	step2[2] = int32(int64(step1[2]) + int64(step1[5]))
	step2[3] = int32(int64(step1[3]) + int64(step1[4]))
	step2[4] = int32(int64(step1[3]) - int64(step1[4]))
	step2[5] = int32(int64(step1[2]) - int64(step1[5]))
	step2[6] = int32(int64(step1[1]) - int64(step1[6]))
	step2[7] = int32(int64(step1[0]) - int64(step1[7]))
	step2[8] = step1[8]
	step2[9] = step1[9]
	temp1 = (-int64(step1[10]) + int64(step1[13])) * cospi16_64
	temp2 = (int64(step1[10]) + int64(step1[13])) * cospi16_64
	step2[10] = int32(dctConstRoundShift(temp1))
	step2[13] = int32(dctConstRoundShift(temp2))
	temp1 = (-int64(step1[11]) + int64(step1[12])) * cospi16_64
	temp2 = (int64(step1[11]) + int64(step1[12])) * cospi16_64
	step2[11] = int32(dctConstRoundShift(temp1))
	step2[12] = int32(dctConstRoundShift(temp2))
	step2[14] = step1[14]
	step2[15] = step1[15]

	step2[16] = int32(int64(step1[16]) + int64(step1[23]))
	step2[17] = int32(int64(step1[17]) + int64(step1[22]))
	step2[18] = int32(int64(step1[18]) + int64(step1[21]))
	step2[19] = int32(int64(step1[19]) + int64(step1[20]))
	step2[20] = int32(int64(step1[19]) - int64(step1[20]))
	step2[21] = int32(int64(step1[18]) - int64(step1[21]))
	step2[22] = int32(int64(step1[17]) - int64(step1[22]))
	step2[23] = int32(int64(step1[16]) - int64(step1[23]))

	step2[24] = int32(-int64(step1[24]) + int64(step1[31]))
	step2[25] = int32(-int64(step1[25]) + int64(step1[30]))
	step2[26] = int32(-int64(step1[26]) + int64(step1[29]))
	step2[27] = int32(-int64(step1[27]) + int64(step1[28]))
	step2[28] = int32(int64(step1[27]) + int64(step1[28]))
	step2[29] = int32(int64(step1[26]) + int64(step1[29]))
	step2[30] = int32(int64(step1[25]) + int64(step1[30]))
	step2[31] = int32(int64(step1[24]) + int64(step1[31]))

	// stage 7
	step1[0] = int32(int64(step2[0]) + int64(step2[15]))
	step1[1] = int32(int64(step2[1]) + int64(step2[14]))
	step1[2] = int32(int64(step2[2]) + int64(step2[13]))
	step1[3] = int32(int64(step2[3]) + int64(step2[12]))
	step1[4] = int32(int64(step2[4]) + int64(step2[11]))
	step1[5] = int32(int64(step2[5]) + int64(step2[10]))
	step1[6] = int32(int64(step2[6]) + int64(step2[9]))
	step1[7] = int32(int64(step2[7]) + int64(step2[8]))
	step1[8] = int32(int64(step2[7]) - int64(step2[8]))
	step1[9] = int32(int64(step2[6]) - int64(step2[9]))
	step1[10] = int32(int64(step2[5]) - int64(step2[10]))
	step1[11] = int32(int64(step2[4]) - int64(step2[11]))
	step1[12] = int32(int64(step2[3]) - int64(step2[12]))
	step1[13] = int32(int64(step2[2]) - int64(step2[13]))
	step1[14] = int32(int64(step2[1]) - int64(step2[14]))
	step1[15] = int32(int64(step2[0]) - int64(step2[15]))

	step1[16] = step2[16]
	step1[17] = step2[17]
	step1[18] = step2[18]
	step1[19] = step2[19]
	temp1 = (-int64(step2[20]) + int64(step2[27])) * cospi16_64
	temp2 = (int64(step2[20]) + int64(step2[27])) * cospi16_64
	step1[20] = int32(dctConstRoundShift(temp1))
	step1[27] = int32(dctConstRoundShift(temp2))
	temp1 = (-int64(step2[21]) + int64(step2[26])) * cospi16_64
	temp2 = (int64(step2[21]) + int64(step2[26])) * cospi16_64
	step1[21] = int32(dctConstRoundShift(temp1))
	step1[26] = int32(dctConstRoundShift(temp2))
	temp1 = (-int64(step2[22]) + int64(step2[25])) * cospi16_64
	temp2 = (int64(step2[22]) + int64(step2[25])) * cospi16_64
	step1[22] = int32(dctConstRoundShift(temp1))
	step1[25] = int32(dctConstRoundShift(temp2))
	temp1 = (-int64(step2[23]) + int64(step2[24])) * cospi16_64
	temp2 = (int64(step2[23]) + int64(step2[24])) * cospi16_64
	step1[23] = int32(dctConstRoundShift(temp1))
	step1[24] = int32(dctConstRoundShift(temp2))
	step1[28] = step2[28]
	step1[29] = step2[29]
	step1[30] = step2[30]
	step1[31] = step2[31]

	// final stage
	output[0] = int32(int64(step1[0]) + int64(step1[31]))
	output[1] = int32(int64(step1[1]) + int64(step1[30]))
	output[2] = int32(int64(step1[2]) + int64(step1[29]))
	output[3] = int32(int64(step1[3]) + int64(step1[28]))
	output[4] = int32(int64(step1[4]) + int64(step1[27]))
	output[5] = int32(int64(step1[5]) + int64(step1[26]))
	output[6] = int32(int64(step1[6]) + int64(step1[25]))
	output[7] = int32(int64(step1[7]) + int64(step1[24]))
	output[8] = int32(int64(step1[8]) + int64(step1[23]))
	output[9] = int32(int64(step1[9]) + int64(step1[22]))
	output[10] = int32(int64(step1[10]) + int64(step1[21]))
	output[11] = int32(int64(step1[11]) + int64(step1[20]))
	output[12] = int32(int64(step1[12]) + int64(step1[19]))
	output[13] = int32(int64(step1[13]) + int64(step1[18]))
	output[14] = int32(int64(step1[14]) + int64(step1[17]))
	output[15] = int32(int64(step1[15]) + int64(step1[16]))
	output[16] = int32(int64(step1[15]) - int64(step1[16]))
	output[17] = int32(int64(step1[14]) - int64(step1[17]))
	output[18] = int32(int64(step1[13]) - int64(step1[18]))
	output[19] = int32(int64(step1[12]) - int64(step1[19]))
	output[20] = int32(int64(step1[11]) - int64(step1[20]))
	output[21] = int32(int64(step1[10]) - int64(step1[21]))
	output[22] = int32(int64(step1[9]) - int64(step1[22]))
	output[23] = int32(int64(step1[8]) - int64(step1[23]))
	output[24] = int32(int64(step1[7]) - int64(step1[24]))
	output[25] = int32(int64(step1[6]) - int64(step1[25]))
	output[26] = int32(int64(step1[5]) - int64(step1[26]))
	output[27] = int32(int64(step1[4]) - int64(step1[27]))
	output[28] = int32(int64(step1[3]) - int64(step1[28]))
	output[29] = int32(int64(step1[2]) - int64(step1[29]))
	output[30] = int32(int64(step1[1]) - int64(step1[30]))
	output[31] = int32(int64(step1[0]) - int64(step1[31]))
}

func iadst4(input, output []int32) {
	x0 := int64(input[0])
	x1 := int64(input[1])
	x2 := int64(input[2])
	x3 := int64(input[3])

	if x0|x1|x2|x3 == 0 {
		output[0] = 0
		output[1] = 0
		output[2] = 0
		output[3] = 0
		return
	}

	s0 := sinpi1_9 * x0
	s1 := sinpi2_9 * x0
	s2 := sinpi3_9 * x1
	s3 := sinpi4_9 * x2
	s4 := sinpi1_9 * x2
	s5 := sinpi2_9 * x3
	s6 := sinpi4_9 * x3
	s7 := int64(int32(x0 - x2 + x3))

	s0 = s0 + s3 + s5
	s1 = s1 - s4 - s6
	s3 = s2
	s2 = sinpi3_9 * s7

	output[0] = int32(dctConstRoundShift(s0 + s3))
	output[1] = int32(dctConstRoundShift(s1 + s3))
	output[2] = int32(dctConstRoundShift(s2))
	output[3] = int32(dctConstRoundShift(s0 + s1 - s3))
}

func iadst8(input, output []int32) {
	x0 := int64(input[7])
	x1 := int64(input[0])
	x2 := int64(input[5])
	x3 := int64(input[2])
	x4 := int64(input[3])
	x5 := int64(input[4])
	x6 := int64(input[1])
	x7 := int64(input[6])

	if x0|x1|x2|x3|x4|x5|x6|x7 == 0 {
		for i := range 8 {
			output[i] = 0
		}
		return
	}

	// stage 1
	s0 := cospi2_64*x0 + cospi30_64*x1
	s1 := cospi30_64*x0 - cospi2_64*x1
	s2 := cospi10_64*x2 + cospi22_64*x3
	s3 := cospi22_64*x2 - cospi10_64*x3
	s4 := cospi18_64*x4 + cospi14_64*x5
	s5 := cospi14_64*x4 - cospi18_64*x5
	s6 := cospi26_64*x6 + cospi6_64*x7
	s7 := cospi6_64*x6 - cospi26_64*x7

	x0 = int64(int32(dctConstRoundShift(s0 + s4)))
	x1 = int64(int32(dctConstRoundShift(s1 + s5)))
	x2 = int64(int32(dctConstRoundShift(s2 + s6)))
	x3 = int64(int32(dctConstRoundShift(s3 + s7)))
	x4 = int64(int32(dctConstRoundShift(s0 - s4)))
	x5 = int64(int32(dctConstRoundShift(s1 - s5)))
	x6 = int64(int32(dctConstRoundShift(s2 - s6)))
	x7 = int64(int32(dctConstRoundShift(s3 - s7)))

	// stage 2
	s0 = x0
	s1 = x1
	s2 = x2
	s3 = x3
	s4 = cospi8_64*x4 + cospi24_64*x5
	s5 = cospi24_64*x4 - cospi8_64*x5
	s6 = -cospi24_64*x6 + cospi8_64*x7
	s7 = cospi8_64*x6 + cospi24_64*x7

	x0 = int64(int32(s0 + s2))
	x1 = int64(int32(s1 + s3))
	x2 = int64(int32(s0 - s2))
	x3 = int64(int32(s1 - s3))
	x4 = int64(int32(dctConstRoundShift(s4 + s6)))
	x5 = int64(int32(dctConstRoundShift(s5 + s7)))
	x6 = int64(int32(dctConstRoundShift(s4 - s6)))
	x7 = int64(int32(dctConstRoundShift(s5 - s7)))

	// stage 3
	s2 = cospi16_64 * (x2 + x3)
	s3 = cospi16_64 * (x2 - x3)
	s6 = cospi16_64 * (x6 + x7)
	s7 = cospi16_64 * (x6 - x7)

	x2 = int64(int32(dctConstRoundShift(s2)))
	x3 = int64(int32(dctConstRoundShift(s3)))
	x6 = int64(int32(dctConstRoundShift(s6)))
	x7 = int64(int32(dctConstRoundShift(s7)))

	output[0] = int32(x0)
	output[1] = -int32(x4)
	output[2] = int32(x6)
	output[3] = -int32(x2)
	output[4] = int32(x3)
	output[5] = -int32(x7)
	output[6] = int32(x5)
	output[7] = -int32(x1)
}

func iadst16(input, output []int32) {
	x0 := int64(input[15])
	x1 := int64(input[0])
	x2 := int64(input[13])
	x3 := int64(input[2])
	x4 := int64(input[11])
	x5 := int64(input[4])
	x6 := int64(input[9])
	x7 := int64(input[6])
	x8 := int64(input[7])
	x9 := int64(input[8])
	x10 := int64(input[5])
	x11 := int64(input[10])
	x12 := int64(input[3])
	x13 := int64(input[12])
	x14 := int64(input[1])
	x15 := int64(input[14])

	if x0|x1|x2|x3|x4|x5|x6|x7|x8|x9|x10|x11|x12|x13|x14|x15 == 0 {
		for i := range 16 {
			output[i] = 0
		}
		return
	}

	// stage 1
	s0 := x0*cospi1_64 + x1*cospi31_64
	s1 := x0*cospi31_64 - x1*cospi1_64
	s2 := x2*cospi5_64 + x3*cospi27_64
	s3 := x2*cospi27_64 - x3*cospi5_64
	s4 := x4*cospi9_64 + x5*cospi23_64
	s5 := x4*cospi23_64 - x5*cospi9_64
	s6 := x6*cospi13_64 + x7*cospi19_64
	s7 := x6*cospi19_64 - x7*cospi13_64
	s8 := x8*cospi17_64 + x9*cospi15_64
	s9 := x8*cospi15_64 - x9*cospi17_64
	s10 := x10*cospi21_64 + x11*cospi11_64
	s11 := x10*cospi11_64 - x11*cospi21_64
	s12 := x12*cospi25_64 + x13*cospi7_64
	s13 := x12*cospi7_64 - x13*cospi25_64
	s14 := x14*cospi29_64 + x15*cospi3_64
	s15 := x14*cospi3_64 - x15*cospi29_64

	x0 = int64(int32(dctConstRoundShift(s0 + s8)))
	x1 = int64(int32(dctConstRoundShift(s1 + s9)))
	x2 = int64(int32(dctConstRoundShift(s2 + s10)))
	x3 = int64(int32(dctConstRoundShift(s3 + s11)))
	x4 = int64(int32(dctConstRoundShift(s4 + s12)))
	x5 = int64(int32(dctConstRoundShift(s5 + s13)))
	x6 = int64(int32(dctConstRoundShift(s6 + s14)))
	x7 = int64(int32(dctConstRoundShift(s7 + s15)))
	x8 = int64(int32(dctConstRoundShift(s0 - s8)))
	x9 = int64(int32(dctConstRoundShift(s1 - s9)))
	x10 = int64(int32(dctConstRoundShift(s2 - s10)))
	x11 = int64(int32(dctConstRoundShift(s3 - s11)))
	x12 = int64(int32(dctConstRoundShift(s4 - s12)))
	x13 = int64(int32(dctConstRoundShift(s5 - s13)))
	x14 = int64(int32(dctConstRoundShift(s6 - s14)))
	x15 = int64(int32(dctConstRoundShift(s7 - s15)))

	// stage 2
	s0 = x0
	s1 = x1
	s2 = x2
	s3 = x3
	s4 = x4
	s5 = x5
	s6 = x6
	s7 = x7
	s8 = x8*cospi4_64 + x9*cospi28_64
	s9 = x8*cospi28_64 - x9*cospi4_64
	s10 = x10*cospi20_64 + x11*cospi12_64
	s11 = x10*cospi12_64 - x11*cospi20_64
	s12 = -x12*cospi28_64 + x13*cospi4_64
	s13 = x12*cospi4_64 + x13*cospi28_64
	s14 = -x14*cospi12_64 + x15*cospi20_64
	s15 = x14*cospi20_64 + x15*cospi12_64

	x0 = int64(int32(s0 + s4))
	x1 = int64(int32(s1 + s5))
	x2 = int64(int32(s2 + s6))
	x3 = int64(int32(s3 + s7))
	x4 = int64(int32(s0 - s4))
	x5 = int64(int32(s1 - s5))
	x6 = int64(int32(s2 - s6))
	x7 = int64(int32(s3 - s7))
	x8 = int64(int32(dctConstRoundShift(s8 + s12)))
	x9 = int64(int32(dctConstRoundShift(s9 + s13)))
	x10 = int64(int32(dctConstRoundShift(s10 + s14)))
	x11 = int64(int32(dctConstRoundShift(s11 + s15)))
	x12 = int64(int32(dctConstRoundShift(s8 - s12)))
	x13 = int64(int32(dctConstRoundShift(s9 - s13)))
	x14 = int64(int32(dctConstRoundShift(s10 - s14)))
	x15 = int64(int32(dctConstRoundShift(s11 - s15)))

	// stage 3
	s0 = x0
	s1 = x1
	s2 = x2
	s3 = x3
	s4 = x4*cospi8_64 + x5*cospi24_64
	s5 = x4*cospi24_64 - x5*cospi8_64
	s6 = -x6*cospi24_64 + x7*cospi8_64
	s7 = x6*cospi8_64 + x7*cospi24_64
	s8 = x8
	s9 = x9
	s10 = x10
	s11 = x11
	s12 = x12*cospi8_64 + x13*cospi24_64
	s13 = x12*cospi24_64 - x13*cospi8_64
	s14 = -x14*cospi24_64 + x15*cospi8_64
	s15 = x14*cospi8_64 + x15*cospi24_64

	x0 = int64(int32(s0 + s2))
	x1 = int64(int32(s1 + s3))
	x2 = int64(int32(s0 - s2))
	x3 = int64(int32(s1 - s3))
	x4 = int64(int32(dctConstRoundShift(s4 + s6)))
	x5 = int64(int32(dctConstRoundShift(s5 + s7)))
	x6 = int64(int32(dctConstRoundShift(s4 - s6)))
	x7 = int64(int32(dctConstRoundShift(s5 - s7)))
	x8 = int64(int32(s8 + s10))
	x9 = int64(int32(s9 + s11))
	x10 = int64(int32(s8 - s10))
	x11 = int64(int32(s9 - s11))
	x12 = int64(int32(dctConstRoundShift(s12 + s14)))
	x13 = int64(int32(dctConstRoundShift(s13 + s15)))
	x14 = int64(int32(dctConstRoundShift(s12 - s14)))
	x15 = int64(int32(dctConstRoundShift(s13 - s15)))

	// stage 4
	s2 = -cospi16_64 * (x2 + x3)
	s3 = cospi16_64 * (x2 - x3)
	s6 = cospi16_64 * (x6 + x7)
	s7 = cospi16_64 * (-x6 + x7)
	s10 = cospi16_64 * (x10 + x11)
	s11 = cospi16_64 * (-x10 + x11)
	s14 = -cospi16_64 * (x14 + x15)
	s15 = cospi16_64 * (x14 - x15)

	x2 = int64(int32(dctConstRoundShift(s2)))
	x3 = int64(int32(dctConstRoundShift(s3)))
	x6 = int64(int32(dctConstRoundShift(s6)))
	x7 = int64(int32(dctConstRoundShift(s7)))
	x10 = int64(int32(dctConstRoundShift(s10)))
	x11 = int64(int32(dctConstRoundShift(s11)))
	x14 = int64(int32(dctConstRoundShift(s14)))
	x15 = int64(int32(dctConstRoundShift(s15)))

	output[0] = int32(x0)
	output[1] = -int32(x8)
	output[2] = int32(x12)
	output[3] = -int32(x4)
	output[4] = int32(x6)
	output[5] = int32(x14)
	output[6] = int32(x10)
	output[7] = int32(x2)
	output[8] = int32(x3)
	output[9] = int32(x11)
	output[10] = int32(x15)
	output[11] = int32(x7)
	output[12] = int32(x5)
	output[13] = -int32(x13)
	output[14] = int32(x9)
	output[15] = -int32(x1)
}

// dctConstRoundShift is libvpx's dct_const_round_shift.
func dctConstRoundShift(input int64) int32 {
	return int32((input + dctConstRounding) >> dctConstBits)
}
