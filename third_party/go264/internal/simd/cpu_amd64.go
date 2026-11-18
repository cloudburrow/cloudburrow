//go:build amd64 && !purego

package simd

func cpuid(eaxArg, ecxArg uint32) (eax, ebx, ecx, edx uint32)

func xgetbv0() (eax, edx uint32)

var (
	hasSSE41 = detectSSE41()
	hasAVX2  = detectAVX2()
)

func detectSSE41() bool {
	maxID, _, _, _ := cpuid(0, 0)
	if maxID < 1 {
		return false
	}
	_, _, ecx, _ := cpuid(1, 0)
	return ecx&(1<<19) != 0
}

func detectAVX2() bool {
	maxID, _, _, _ := cpuid(0, 0)
	if maxID < 7 {
		return false
	}
	_, _, ecx1, _ := cpuid(1, 0)
	const osxsave = 1 << 27
	const avx = 1 << 28
	if ecx1&osxsave == 0 || ecx1&avx == 0 {
		return false
	}
	xcr0, _ := xgetbv0()
	if xcr0&6 != 6 {
		return false
	}
	_, ebx7, _, _ := cpuid(7, 0)
	return ebx7&(1<<5) != 0
}
