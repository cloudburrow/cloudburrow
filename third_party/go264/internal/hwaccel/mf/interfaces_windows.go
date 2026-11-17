package mf

import "unsafe"

var (
	IIDIMFTransform           = GUID{Data1: 0xbf94c121, Data2: 0x5b05, Data3: 0x4e6f, Data4: [8]byte{0x80, 0x00, 0xba, 0x59, 0x89, 0x61, 0x41, 0x4d}}
	IIDIMFMediaType           = GUID{Data1: 0x44ae0fa8, Data2: 0xea31, Data3: 0x4109, Data4: [8]byte{0x8d, 0x2e, 0x4c, 0xae, 0x49, 0x97, 0xc5, 0x55}}
	IIDIMFAttributes          = GUID{Data1: 0x2cd2d921, Data2: 0xc447, Data3: 0x44a7, Data4: [8]byte{0xa1, 0x3c, 0x4a, 0xda, 0xbf, 0xc2, 0x47, 0xe3}}
	IIDIMFSample              = GUID{Data1: 0xc40a00f2, Data2: 0xb93a, Data3: 0x4d80, Data4: [8]byte{0xae, 0x8c, 0x5a, 0x1c, 0x63, 0x4f, 0x58, 0xe4}}
	IIDIMFMediaBuffer         = GUID{Data1: 0x045fa593, Data2: 0x8799, Data3: 0x42b8, Data4: [8]byte{0xbc, 0x8d, 0x89, 0x68, 0xc6, 0x45, 0x35, 0x07}}
	IIDIMFActivate            = GUID{Data1: 0x7fee9e9a, Data2: 0x4a89, Data3: 0x47a6, Data4: [8]byte{0x89, 0x9c, 0xb6, 0xa5, 0x3a, 0x70, 0xfb, 0x67}}
	IIDIMFMediaEventGenerator = GUID{Data1: 0x2cd0bd52, Data2: 0xbcd5, Data3: 0x4b89, Data4: [8]byte{0xb6, 0x2c, 0xea, 0xdc, 0x0c, 0x03, 0x1e, 0x7d}}
	IIDIMFShutdown            = GUID{Data1: 0x97ec2ea4, Data2: 0x0e42, Data3: 0x4937, Data4: [8]byte{0x97, 0xac, 0x9d, 0x6d, 0x32, 0x88, 0x24, 0xe1}}
	IIDIMFMediaEvent          = GUID{Data1: 0xdf598932, Data2: 0xf10c, Data3: 0x4e39, Data4: [8]byte{0xbb, 0xa2, 0xc3, 0x08, 0xf1, 0x01, 0xda, 0xa3}}
	IIDICodecAPI              = GUID{Data1: 0x901db4c7, Data2: 0x31ce, Data3: 0x41a2, Data4: [8]byte{0x85, 0xdc, 0x8f, 0xa0, 0xbf, 0x41, 0xb8, 0xda}}
)

var (
	MFMTMajorType               = GUID{Data1: 0x48eba18e, Data2: 0xf8c9, Data3: 0x4687, Data4: [8]byte{0xbf, 0x11, 0x0a, 0x74, 0xc9, 0xf9, 0x6a, 0x8f}}
	MFMTSubtype                 = GUID{Data1: 0xf7e34c9a, Data2: 0x42e8, Data3: 0x4714, Data4: [8]byte{0xb7, 0x4b, 0xcb, 0x29, 0xd7, 0x2c, 0x35, 0xe5}}
	MFMTFrameSize               = GUID{Data1: 0x1652c33d, Data2: 0xd6b2, Data3: 0x4012, Data4: [8]byte{0xb8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xa3, 0x7d}}
	MFMTFrameRate               = GUID{Data1: 0xc459a2e8, Data2: 0x3d2c, Data3: 0x4e44, Data4: [8]byte{0xb1, 0x32, 0xfe, 0xe5, 0x15, 0x6c, 0x7b, 0xb0}}
	MFMTAvgBitrate              = GUID{Data1: 0x20332624, Data2: 0xfb0d, Data3: 0x4d9e, Data4: [8]byte{0xbd, 0x0d, 0xcb, 0xf6, 0x78, 0x6c, 0x10, 0x2e}}
	MFMTInterlaceMode           = GUID{Data1: 0xe2724bb8, Data2: 0xe676, Data3: 0x4806, Data4: [8]byte{0xb4, 0xb2, 0xa8, 0xd6, 0xef, 0xb4, 0x4c, 0xcd}}
	MFMTPixelAspectRatio        = GUID{Data1: 0xc6376a1e, Data2: 0x8d0a, Data3: 0x4027, Data4: [8]byte{0xbe, 0x45, 0x6d, 0x9a, 0x0a, 0xd3, 0x9b, 0xb6}}
	MFMTAllSamplesIndependent   = GUID{Data1: 0xc9173739, Data2: 0x5e56, Data3: 0x461c, Data4: [8]byte{0xb7, 0x13, 0x46, 0xfb, 0x99, 0x5c, 0xb9, 0x5f}}
	MFMTMpeg2Profile            = GUID{Data1: 0xad76a80b, Data2: 0x2d5c, Data3: 0x4e0b, Data4: [8]byte{0xb3, 0x75, 0x64, 0xe5, 0x20, 0x13, 0x70, 0x36}}
	MFMTMpeg2Level              = GUID{Data1: 0x96f66574, Data2: 0x11c5, Data3: 0x4015, Data4: [8]byte{0x86, 0x66, 0xbf, 0xf5, 0x16, 0x43, 0x6d, 0xa7}}
	MFMTDefaultStride           = GUID{Data1: 0x644b4e48, Data2: 0x1e02, Data3: 0x4516, Data4: [8]byte{0xb0, 0xeb, 0xc0, 0x1c, 0xa9, 0xd4, 0x9a, 0xc6}}
	MFMTMinimumDisplayAperture  = GUID{Data1: 0xd7388766, Data2: 0x18fe, Data3: 0x48c6, Data4: [8]byte{0xa1, 0x77, 0xee, 0x89, 0x48, 0x67, 0xc8, 0xc4}}
	MFMTMpegSequenceHeader      = GUID{Data1: 0x3c036de7, Data2: 0x3ad0, Data3: 0x4c9e, Data4: [8]byte{0x92, 0x16, 0xee, 0x6d, 0x6a, 0xc2, 0x1c, 0xb3}}
	MFTransformAsync            = GUID{Data1: 0xf81a699a, Data2: 0x649a, Data3: 0x497d, Data4: [8]byte{0x8c, 0x73, 0x29, 0xf8, 0xfe, 0xd6, 0xad, 0x7a}}
	MFTransformAsyncUnlock      = GUID{Data1: 0xe5666d6b, Data2: 0x3422, Data3: 0x4eb6, Data4: [8]byte{0xa4, 0x21, 0xda, 0x7d, 0xb1, 0xf8, 0xe2, 0x07}}
	MFTFriendlyNameAttribute    = GUID{Data1: 0x314ffbae, Data2: 0x5b41, Data3: 0x4c95, Data4: [8]byte{0x9c, 0x19, 0x4e, 0x7d, 0x58, 0x6f, 0xac, 0xe3}}
	MFTEnumHardwareURLAttribute = GUID{Data1: 0x2fb866ac, Data2: 0xb078, Data3: 0x4942, Data4: [8]byte{0xab, 0x6c, 0x00, 0x3d, 0x05, 0xcd, 0xa6, 0x74}}
	MFSAD3D11Aware              = GUID{Data1: 0x206b4fc8, Data2: 0xfcf9, Data3: 0x4c51, Data4: [8]byte{0xaf, 0xe3, 0x97, 0x64, 0x36, 0x9e, 0x33, 0xa0}}
	MFLowLatency                = GUID{Data1: 0x9c27891a, Data2: 0xed7a, Data3: 0x40e1, Data4: [8]byte{0x88, 0xe8, 0xb2, 0x27, 0x27, 0xa0, 0x24, 0xee}}
)

var (
	MFTCategoryVideoDecoder = GUID{Data1: 0xd6c02d4b, Data2: 0x6833, Data3: 0x45b4, Data4: [8]byte{0x97, 0x1a, 0x05, 0xa4, 0xb0, 0x4b, 0xab, 0x91}}
	MFTCategoryVideoEncoder = GUID{Data1: 0xf79eac7d, Data2: 0xe545, Data3: 0x4387, Data4: [8]byte{0xbd, 0xee, 0xd6, 0x47, 0xd7, 0xbd, 0xe4, 0x2a}}
)

const (
	MFTMessageCommandFlush         = 0x0
	MFTMessageCommandDrain         = 0x1
	MFTMessageSetD3DManager        = 0x2
	MFTMessageNotifyBeginStreaming = 0x10000000
	MFTMessageNotifyEndStreaming   = 0x10000001
	MFTMessageNotifyEndOfStream    = 0x10000002
	MFTMessageNotifyStartOfStream  = 0x10000003
)

const (
	MFTEnumFlagSyncMFT       = 0x00000001
	MFTEnumFlagAsyncMFT      = 0x00000002
	MFTEnumFlagHardware      = 0x00000004
	MFTEnumFlagSortAndFilter = 0x00000040
)

const (
	MFTOutputStatusSampleReady    = 0x1
	MFTOutputDataBufferIncomplete = 0x1000000
)

const (
	MFETransformNeedMoreInput HRESULT = 0xc00d6d72
	MFETransformStreamChange  HRESULT = 0xc00d6d61
	MFEInvalidMediaType       HRESULT = 0xc00d36b4
	MFENotAccepting           HRESULT = 0xc00d36b5
	MFENoMoreTypes            HRESULT = 0xc00d36b9
)

const MFVideoInterlaceProgressive = 2

const (
	AVEncH264VProfileBase = 66
	AVEncH264VProfileMain = 77
	AVEncH264VProfileHigh = 100
)

type MFTOutputDataBuffer struct {
	DwStreamID uint32
	PSample    unsafe.Pointer
	DwStatus   uint32
	PEvents    unsafe.Pointer
}

type MFTInputStreamInfo struct {
	HnsMaxLatency  int64
	DwFlags        uint32
	CbSize         uint32
	CbMaxLookahead uint32
	CbAlignment    uint32
}

type MFTOutputStreamInfo struct {
	DwFlags     uint32
	CbSize      uint32
	CbAlignment uint32
}

type MFTRegisterTypeInfo struct {
	GuidMajorType GUID
	GuidSubtype   GUID
}

const (
	attributesGetItem            = 3
	attributesGetItemType        = 4
	attributesCompareItem        = 5
	attributesCompare            = 6
	attributesGetUINT32          = 7
	attributesGetUINT64          = 8
	attributesGetDouble          = 9
	attributesGetGUID            = 10
	attributesGetStringLength    = 11
	attributesGetString          = 12
	attributesGetAllocatedString = 13
	attributesGetBlobSize        = 14
	attributesGetBlob            = 15
	attributesGetAllocatedBlob   = 16
	attributesGetUnknown         = 17
	attributesSetItem            = 18
	attributesDeleteItem         = 19
	attributesDeleteAllItems     = 20
	attributesSetUINT32          = 21
	attributesSetUINT64          = 22
	attributesSetDouble          = 23
	attributesSetGUID            = 24
	attributesSetString          = 25
	attributesSetBlob            = 26
	attributesSetUnknown         = 27
	attributesLockStore          = 28
	attributesUnlockStore        = 29
	attributesGetCount           = 30
	attributesGetItemByIndex     = 31
	attributesCopyAllItems       = 32
)

const (
	mediaTypeGetMajorType       = 33
	mediaTypeIsCompressedFormat = 34
	mediaTypeIsEqual            = 35
	mediaTypeGetRepresentation  = 36
	mediaTypeFreeRepresentation = 37
)

const (
	sampleGetSampleFlags            = 33
	sampleSetSampleFlags            = 34
	sampleGetSampleTime             = 35
	sampleSetSampleTime             = 36
	sampleGetSampleDuration         = 37
	sampleSetSampleDuration         = 38
	sampleGetBufferCount            = 39
	sampleGetBufferByIndex          = 40
	sampleConvertToContiguousBuffer = 41
	sampleAddBuffer                 = 42
	sampleRemoveBufferByIndex       = 43
	sampleRemoveAllBuffers          = 44
	sampleGetTotalLength            = 45
	sampleCopyToBuffer              = 46
)

const (
	bufferLock             = 3
	bufferUnlock           = 4
	bufferGetCurrentLength = 5
	bufferSetCurrentLength = 6
	bufferGetMaxLength     = 7
)

const (
	activateActivateObject = 33
	activateShutdownObject = 34
	activateDetachObject   = 35
)

const shutdownShutdown = 3

const (
	eventGeneratorGetEvent      = 3
	eventGeneratorBeginGetEvent = 4
	eventGeneratorEndGetEvent   = 5
	eventGeneratorQueueEvent    = 6
)

const (
	mediaEventGetType         = 33
	mediaEventGetExtendedType = 34
	mediaEventGetStatus       = 35
	mediaEventGetValue        = 36
)

const (
	transformGetStreamLimits           = 3
	transformGetStreamCount            = 4
	transformGetStreamIDs              = 5
	transformGetInputStreamInfo        = 6
	transformGetOutputStreamInfo       = 7
	transformGetAttributes             = 8
	transformGetInputStreamAttributes  = 9
	transformGetOutputStreamAttributes = 10
	transformDeleteInputStream         = 11
	transformAddInputStreams           = 12
	transformGetInputAvailableType     = 13
	transformGetOutputAvailableType    = 14
	transformSetInputType              = 15
	transformSetOutputType             = 16
	transformGetInputCurrentType       = 17
	transformGetOutputCurrentType      = 18
	transformGetInputStatus            = 19
	transformGetOutputStatus           = 20
	transformSetOutputBounds           = 21
	transformProcessEvent              = 22
	transformProcessMessage            = 23
	transformProcessInput              = 24
	transformProcessOutput             = 25
)
