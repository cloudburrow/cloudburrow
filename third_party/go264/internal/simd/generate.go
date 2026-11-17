package simd

//go:generate go -C asmgen run . -pkg simd -out ../kernels_amd64.s -stubs ../kernels_amd64.go
