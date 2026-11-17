package syntax

import (
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
)

const (
	SEIPayloadTypeBufferingPeriod      = 0
	SEIPayloadTypePicTiming            = 1
	SEIPayloadTypeUserDataUnregistered = 5
	SEIPayloadTypeRecoveryPoint        = 6
)

var seiNumClockTS = [9]int{1, 1, 1, 2, 2, 3, 3, 2, 3}

type SEIRecoveryPoint struct {
	RecoveryFrameCnt      uint32
	ExactMatch            bool
	BrokenLink            bool
	ChangingSliceGroupIDC uint8
}

type SEIInitialCPBDelay struct {
	InitialCPBRemovalDelay       uint32
	InitialCPBRemovalDelayOffset uint32
}

type SEIBufferingPeriod struct {
	SeqParameterSetID uint32
	NalHRD            []SEIInitialCPBDelay
	VclHRD            []SEIInitialCPBDelay
}

type SEIClockTimestamp struct {
	Present        bool
	CTType         uint8
	NuitFieldBased bool
	CountingType   uint8
	FullTimestamp  bool
	Discontinuity  bool
	CntDropped     bool
	NFrames        uint8
	SecondsFlag    bool
	Seconds        uint8
	MinutesFlag    bool
	Minutes        uint8
	HoursFlag      bool
	Hours          uint8
	TimeOffset     int32
}

type SEIPicTiming struct {
	CPBRemovalDelay uint32
	DPBOutputDelay  uint32
	PicStruct       uint8
	ClockTimestamps []SEIClockTimestamp
}

type SEIUserDataUnregistered struct {
	UUID [16]byte
	Data []byte
}

type SEIMessage struct {
	PayloadType uint32
	Opaque      []byte

	RecoveryPoint        *SEIRecoveryPoint
	BufferingPeriod      *SEIBufferingPeriod
	PicTiming            *SEIPicTiming
	UserDataUnregistered *SEIUserDataUnregistered
}

func readSEIExtended(r *bits.Reader) (uint32, error) {
	var total uint64
	for {
		v, err := r.ReadBits(8)
		if err != nil {
			return 0, err
		}
		total += uint64(v)
		if v != 0xFF {
			break
		}
		if total > 0xFFFFFFFF {
			return 0, fmt.Errorf("%w: sei_message type/size overflow", ErrInvalidValue)
		}
	}
	return uint32(total), nil
}

func writeSEIExtended(w *bits.Writer, v uint32) {
	for v >= 255 {
		w.WriteBits(0xFF, 8)
		v -= 255
	}
	w.WriteBits(v, 8)
}

func finishSEIPayload(r *bits.Reader) error {
	if !r.ByteAligned() {
		if err := r.ReadRBSPTrailingBits(); err != nil {
			return err
		}
	}
	if r.BitsLeft() != 0 {
		return fmt.Errorf("%w: sei payload has unconsumed data", ErrInvalidValue)
	}
	return nil
}

func finishSEIPayloadWrite(w *bits.Writer) {
	if !w.ByteAligned() {
		w.WriteRBSPTrailingBits()
	}
}

func decodeSEIRecoveryPoint(payload []byte) (*SEIRecoveryPoint, error) {
	r := bits.NewReader(payload)
	p := &SEIRecoveryPoint{}
	v, err := r.ReadUE()
	if err != nil {
		return nil, err
	}
	if v > 65535 {
		return nil, fmt.Errorf("%w: recovery_frame_cnt %d", ErrInvalidValue, v)
	}
	p.RecoveryFrameCnt = v
	if p.ExactMatch, err = r.ReadFlag(); err != nil {
		return nil, err
	}
	if p.BrokenLink, err = r.ReadFlag(); err != nil {
		return nil, err
	}
	cs, err := r.ReadBits(2)
	if err != nil {
		return nil, err
	}
	p.ChangingSliceGroupIDC = uint8(cs)
	if err := finishSEIPayload(r); err != nil {
		return nil, err
	}
	return p, nil
}

func encodeSEIRecoveryPoint(p *SEIRecoveryPoint) []byte {
	w := bits.NewWriterSize(4)
	w.WriteUE(p.RecoveryFrameCnt)
	w.WriteFlag(p.ExactMatch)
	w.WriteFlag(p.BrokenLink)
	w.WriteBits(uint32(p.ChangingSliceGroupIDC), 2)
	finishSEIPayloadWrite(w)
	return w.Bytes()
}

func decodeSEIInitialCPBDelays(r *bits.Reader, h *HRD) ([]SEIInitialCPBDelay, error) {
	n := int(h.CPBCntMinus1) + 1
	length := int(h.InitialCPBRemovalDelayLengthMinus1) + 1
	out := make([]SEIInitialCPBDelay, n)
	for i := range out {
		v, err := r.ReadBits(length)
		if err != nil {
			return nil, err
		}
		out[i].InitialCPBRemovalDelay = v
		if v, err = r.ReadBits(length); err != nil {
			return nil, err
		}
		out[i].InitialCPBRemovalDelayOffset = v
	}
	return out, nil
}

func encodeSEIInitialCPBDelays(w *bits.Writer, delays []SEIInitialCPBDelay, h *HRD) error {
	n := int(h.CPBCntMinus1) + 1
	if len(delays) != n {
		return fmt.Errorf("%w: buffering period schedule count %d, want %d", ErrInvalidValue, len(delays), n)
	}
	length := int(h.InitialCPBRemovalDelayLengthMinus1) + 1
	for _, d := range delays {
		w.WriteBits(d.InitialCPBRemovalDelay, length)
		w.WriteBits(d.InitialCPBRemovalDelayOffset, length)
	}
	return nil
}

func decodeSEIBufferingPeriod(payload []byte, lookupSPS func(uint32) *SPS) (*SEIBufferingPeriod, error) {
	if lookupSPS == nil {
		return nil, ErrMissingSPS
	}
	r := bits.NewReader(payload)
	bp := &SEIBufferingPeriod{}
	id, err := r.ReadUE()
	if err != nil {
		return nil, err
	}
	if id > 31 {
		return nil, fmt.Errorf("%w: seq_parameter_set_id %d", ErrInvalidValue, id)
	}
	bp.SeqParameterSetID = id
	sps := lookupSPS(id)
	if sps == nil {
		return nil, ErrMissingSPS
	}
	if sps.VUI.NalHRDPresent {
		if bp.NalHRD, err = decodeSEIInitialCPBDelays(r, &sps.VUI.NalHRD); err != nil {
			return nil, err
		}
	}
	if sps.VUI.VclHRDPresent {
		if bp.VclHRD, err = decodeSEIInitialCPBDelays(r, &sps.VUI.VclHRD); err != nil {
			return nil, err
		}
	}
	if err := finishSEIPayload(r); err != nil {
		return nil, err
	}
	return bp, nil
}

func encodeSEIBufferingPeriod(bp *SEIBufferingPeriod, lookupSPS func(uint32) *SPS) ([]byte, error) {
	if lookupSPS == nil {
		return nil, ErrMissingSPS
	}
	sps := lookupSPS(bp.SeqParameterSetID)
	if sps == nil {
		return nil, ErrMissingSPS
	}
	w := bits.NewWriterSize(16)
	w.WriteUE(bp.SeqParameterSetID)
	if sps.VUI.NalHRDPresent {
		if err := encodeSEIInitialCPBDelays(w, bp.NalHRD, &sps.VUI.NalHRD); err != nil {
			return nil, err
		}
	}
	if sps.VUI.VclHRDPresent {
		if err := encodeSEIInitialCPBDelays(w, bp.VclHRD, &sps.VUI.VclHRD); err != nil {
			return nil, err
		}
	}
	finishSEIPayloadWrite(w)
	if err := w.Err(); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}

func picTimingHRD(v *VUI) (hrd *HRD, timeOffsetLength int) {
	switch {
	case v.NalHRDPresent:
		return &v.NalHRD, int(v.NalHRD.TimeOffsetLength)
	case v.VclHRDPresent:
		return &v.VclHRD, int(v.VclHRD.TimeOffsetLength)
	default:
		return nil, 24
	}
}

func decodeSEIClockTimestamp(r *bits.Reader, ct *SEIClockTimestamp, timeOffsetLength int) error {
	v, err := r.ReadBits(2)
	if err != nil {
		return err
	}
	ct.CTType = uint8(v)
	if ct.NuitFieldBased, err = r.ReadFlag(); err != nil {
		return err
	}
	if v, err = r.ReadBits(5); err != nil {
		return err
	}
	ct.CountingType = uint8(v)
	if ct.FullTimestamp, err = r.ReadFlag(); err != nil {
		return err
	}
	if ct.Discontinuity, err = r.ReadFlag(); err != nil {
		return err
	}
	if ct.CntDropped, err = r.ReadFlag(); err != nil {
		return err
	}
	if v, err = r.ReadBits(8); err != nil {
		return err
	}
	ct.NFrames = uint8(v)
	if ct.FullTimestamp {
		if v, err = r.ReadBits(6); err != nil {
			return err
		}
		ct.Seconds = uint8(v)
		if v, err = r.ReadBits(6); err != nil {
			return err
		}
		ct.Minutes = uint8(v)
		if v, err = r.ReadBits(5); err != nil {
			return err
		}
		ct.Hours = uint8(v)
	} else {
		if ct.SecondsFlag, err = r.ReadFlag(); err != nil {
			return err
		}
		if ct.SecondsFlag {
			if v, err = r.ReadBits(6); err != nil {
				return err
			}
			ct.Seconds = uint8(v)
			if ct.MinutesFlag, err = r.ReadFlag(); err != nil {
				return err
			}
			if ct.MinutesFlag {
				if v, err = r.ReadBits(6); err != nil {
					return err
				}
				ct.Minutes = uint8(v)
				if ct.HoursFlag, err = r.ReadFlag(); err != nil {
					return err
				}
				if ct.HoursFlag {
					if v, err = r.ReadBits(5); err != nil {
						return err
					}
					ct.Hours = uint8(v)
				}
			}
		}
	}
	if timeOffsetLength > 0 {
		to, err := readSigned(r, timeOffsetLength)
		if err != nil {
			return err
		}
		ct.TimeOffset = to
	}
	return nil
}

func encodeSEIClockTimestamp(w *bits.Writer, ct *SEIClockTimestamp, timeOffsetLength int) {
	w.WriteBits(uint32(ct.CTType), 2)
	w.WriteFlag(ct.NuitFieldBased)
	w.WriteBits(uint32(ct.CountingType), 5)
	w.WriteFlag(ct.FullTimestamp)
	w.WriteFlag(ct.Discontinuity)
	w.WriteFlag(ct.CntDropped)
	w.WriteBits(uint32(ct.NFrames), 8)
	if ct.FullTimestamp {
		w.WriteBits(uint32(ct.Seconds), 6)
		w.WriteBits(uint32(ct.Minutes), 6)
		w.WriteBits(uint32(ct.Hours), 5)
	} else {
		w.WriteFlag(ct.SecondsFlag)
		if ct.SecondsFlag {
			w.WriteBits(uint32(ct.Seconds), 6)
			w.WriteFlag(ct.MinutesFlag)
			if ct.MinutesFlag {
				w.WriteBits(uint32(ct.Minutes), 6)
				w.WriteFlag(ct.HoursFlag)
				if ct.HoursFlag {
					w.WriteBits(uint32(ct.Hours), 5)
				}
			}
		}
	}
	if timeOffsetLength > 0 {
		writeSigned(w, ct.TimeOffset, timeOffsetLength)
	}
}

func decodeSEIPicTiming(payload []byte, sps *SPS) (*SEIPicTiming, error) {
	if sps == nil {
		return nil, ErrMissingSPS
	}
	r := bits.NewReader(payload)
	pt := &SEIPicTiming{}
	v := &sps.VUI
	if v.NalHRDPresent || v.VclHRDPresent {
		hrd, _ := picTimingHRD(v)
		cr, err := r.ReadBits(int(hrd.CPBRemovalDelayLengthMinus1) + 1)
		if err != nil {
			return nil, err
		}
		pt.CPBRemovalDelay = cr
		dr, err := r.ReadBits(int(hrd.DPBOutputDelayLengthMinus1) + 1)
		if err != nil {
			return nil, err
		}
		pt.DPBOutputDelay = dr
	}
	if v.PicStructPresent {
		ps, err := r.ReadBits(4)
		if err != nil {
			return nil, err
		}
		pt.PicStruct = uint8(ps)
		if int(pt.PicStruct) >= len(seiNumClockTS) {
			return nil, fmt.Errorf("%w: pic_struct %d", ErrInvalidValue, pt.PicStruct)
		}
		_, timeOffsetLength := picTimingHRD(v)
		pt.ClockTimestamps = make([]SEIClockTimestamp, seiNumClockTS[pt.PicStruct])
		for i := range pt.ClockTimestamps {
			present, err := r.ReadFlag()
			if err != nil {
				return nil, err
			}
			if !present {
				continue
			}
			pt.ClockTimestamps[i].Present = true
			if err := decodeSEIClockTimestamp(r, &pt.ClockTimestamps[i], timeOffsetLength); err != nil {
				return nil, err
			}
		}
	}
	if err := finishSEIPayload(r); err != nil {
		return nil, err
	}
	return pt, nil
}

func encodeSEIPicTiming(pt *SEIPicTiming, sps *SPS) ([]byte, error) {
	if sps == nil {
		return nil, ErrMissingSPS
	}
	v := &sps.VUI
	w := bits.NewWriterSize(16)
	if v.NalHRDPresent || v.VclHRDPresent {
		hrd, _ := picTimingHRD(v)
		w.WriteBits(pt.CPBRemovalDelay, int(hrd.CPBRemovalDelayLengthMinus1)+1)
		w.WriteBits(pt.DPBOutputDelay, int(hrd.DPBOutputDelayLengthMinus1)+1)
	}
	if v.PicStructPresent {
		if int(pt.PicStruct) >= len(seiNumClockTS) {
			return nil, fmt.Errorf("%w: pic_struct %d", ErrInvalidValue, pt.PicStruct)
		}
		w.WriteBits(uint32(pt.PicStruct), 4)
		_, timeOffsetLength := picTimingHRD(v)
		want := seiNumClockTS[pt.PicStruct]
		if len(pt.ClockTimestamps) != want {
			return nil, fmt.Errorf("%w: pic_timing clock timestamp count %d, want %d", ErrInvalidValue, len(pt.ClockTimestamps), want)
		}
		for i := range pt.ClockTimestamps {
			ct := &pt.ClockTimestamps[i]
			w.WriteFlag(ct.Present)
			if ct.Present {
				encodeSEIClockTimestamp(w, ct, timeOffsetLength)
			}
		}
	}
	finishSEIPayloadWrite(w)
	if err := w.Err(); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}

func decodeSEIUserDataUnregistered(payload []byte) (*SEIUserDataUnregistered, error) {
	if len(payload) < 16 {
		return nil, fmt.Errorf("%w: user_data_unregistered payload too short", ErrInvalidValue)
	}
	ud := &SEIUserDataUnregistered{}
	copy(ud.UUID[:], payload[:16])
	if len(payload) > 16 {
		ud.Data = append([]byte(nil), payload[16:]...)
	}
	return ud, nil
}

func encodeSEIUserDataUnregistered(ud *SEIUserDataUnregistered) []byte {
	out := make([]byte, 16+len(ud.Data))
	copy(out, ud.UUID[:])
	copy(out[16:], ud.Data)
	return out
}

func readSigned(r *bits.Reader, n int) (int32, error) {
	if n <= 0 {
		return 0, nil
	}
	v, err := r.ReadBits(n)
	if err != nil {
		return 0, err
	}
	if v&(1<<uint(n-1)) != 0 {
		return int32(v) - int32(1)<<uint(n), nil
	}
	return int32(v), nil
}

func writeSigned(w *bits.Writer, v int32, n int) {
	if n <= 0 {
		return
	}
	w.WriteBits(uint32(v)&(1<<uint(n)-1), n)
}

func parseSEIMessage(r *bits.Reader, activeSPS *SPS, lookupSPS func(uint32) *SPS) (SEIMessage, error) {
	payloadType, err := readSEIExtended(r)
	if err != nil {
		return SEIMessage{}, err
	}
	payloadSize, err := readSEIExtended(r)
	if err != nil {
		return SEIMessage{}, err
	}
	if uint64(payloadSize) > uint64(r.BitsLeft()/8) {
		return SEIMessage{}, fmt.Errorf("%w: sei_message payload_size %d exceeds remaining data", ErrInvalidValue, payloadSize)
	}
	payload := make([]byte, payloadSize)
	for i := range payload {
		v, err := r.ReadBits(8)
		if err != nil {
			return SEIMessage{}, err
		}
		payload[i] = byte(v)
	}
	msg := SEIMessage{PayloadType: payloadType}
	switch payloadType {
	case SEIPayloadTypeRecoveryPoint:
		if rp, err := decodeSEIRecoveryPoint(payload); err == nil {
			msg.RecoveryPoint = rp
		} else {
			msg.Opaque = payload
		}
	case SEIPayloadTypeBufferingPeriod:
		if bp, err := decodeSEIBufferingPeriod(payload, lookupSPS); err == nil {
			msg.BufferingPeriod = bp
		} else {
			msg.Opaque = payload
		}
	case SEIPayloadTypePicTiming:
		if pt, err := decodeSEIPicTiming(payload, activeSPS); err == nil {
			msg.PicTiming = pt
		} else {
			msg.Opaque = payload
		}
	case SEIPayloadTypeUserDataUnregistered:
		if ud, err := decodeSEIUserDataUnregistered(payload); err == nil {
			msg.UserDataUnregistered = ud
		} else {
			msg.Opaque = payload
		}
	default:
		msg.Opaque = payload
	}
	return msg, nil
}

func encodeSEIPayload(msg *SEIMessage, activeSPS *SPS, lookupSPS func(uint32) *SPS) ([]byte, error) {
	switch {
	case msg.RecoveryPoint != nil:
		return encodeSEIRecoveryPoint(msg.RecoveryPoint), nil
	case msg.BufferingPeriod != nil:
		return encodeSEIBufferingPeriod(msg.BufferingPeriod, lookupSPS)
	case msg.PicTiming != nil:
		return encodeSEIPicTiming(msg.PicTiming, activeSPS)
	case msg.UserDataUnregistered != nil:
		return encodeSEIUserDataUnregistered(msg.UserDataUnregistered), nil
	default:
		return msg.Opaque, nil
	}
}

func ParseSEI(rbsp []byte, activeSPS *SPS, lookupSPS func(id uint32) *SPS) ([]SEIMessage, error) {
	r := bits.NewReader(rbsp)
	var messages []SEIMessage
	for {
		msg, err := parseSEIMessage(r, activeSPS, lookupSPS)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
		if !r.MoreRBSPData() {
			break
		}
	}
	if err := r.ReadRBSPTrailingBits(); err != nil {
		return nil, err
	}
	return messages, nil
}

func WriteSEI(messages []SEIMessage, activeSPS *SPS, lookupSPS func(id uint32) *SPS) ([]byte, error) {
	w := bits.NewWriterSize(64)
	for i := range messages {
		msg := &messages[i]
		payload, err := encodeSEIPayload(msg, activeSPS, lookupSPS)
		if err != nil {
			return nil, err
		}
		writeSEIExtended(w, msg.PayloadType)
		writeSEIExtended(w, uint32(len(payload)))
		for _, b := range payload {
			w.WriteBits(uint32(b), 8)
		}
	}
	w.WriteRBSPTrailingBits()
	if err := w.Err(); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}
