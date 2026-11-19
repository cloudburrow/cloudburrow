package aac

// program_config_element() (ISO/IEC 14496-3 §4.4.1.1, Table 4.2), for
// channel configuration 0 (#1651): the elements a raw_data_block carries,
// by kind and element_instance_tag, and where each channel sits. Written
// for CloudBurrow from the standard's syntax.

import (
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/aac-go/internal/bits"
)

// pceElement is one element a program_config_element lists.
type pceElement struct {
	cpe, lfe bool
	tag      int
	names    []string // its channels' speakers, as ffmpeg names them
}

// programConfig is a parsed program_config_element.
type programConfig struct {
	elems    []pceElement
	channels int
}

// parsePCE reads a program_config_element, through its comment field.
func parsePCE(r *bits.BitReader) (*programConfig, error) {
	b := &bitr{r: r}
	b.u(4 + 2 + 4) // element_instance_tag, object_type, sampling_frequency_index
	numFront, numSide, numBack := b.u(4), b.u(4), b.u(4)
	numLFE, numAssoc, numCC := b.u(2), b.u(3), b.u(4)
	if b.u(1) == 1 { // mono_mixdown_present
		b.u(4)
	}
	if b.u(1) == 1 { // stereo_mixdown_present
		b.u(4)
	}
	if b.u(1) == 1 { // matrix_mixdown_idx_present
		b.u(2 + 1)
	}
	pc := &programConfig{}
	group := func(n int) []pceElement {
		var es []pceElement
		for range n {
			cpe := b.u(1) == 1
			es = append(es, pceElement{cpe: cpe, tag: b.u(4)})
		}
		return es
	}
	front, side, back := group(numFront), group(numSide), group(numBack)
	var lfe []pceElement
	for range numLFE {
		lfe = append(lfe, pceElement{lfe: true, tag: b.u(4)})
	}
	b.u(4 * numAssoc) // assoc_data_element_tag_select
	b.u(5 * numCC)    // cc_element_is_ind_sw, valid_cc_element_tag_select
	if b.err != nil {
		return nil, fmt.Errorf("aac: program_config_element: %w", b.err)
	}
	alignReader(r)
	comment := b.u(8)
	for range comment {
		b.u(8)
	}
	if b.err != nil {
		return nil, fmt.Errorf("aac: program_config_element: %w", b.err)
	}
	// The speakers: in front, a single centre first, then the pairs from
	// the centre out (ffmpeg's FL/FR and FLC/FRC, which a mono downmix
	// weighs alike); a side pair; behind, pairs and a back centre; the LFE.
	name := func(where string, es []pceElement) error {
		pairs, singles := 0, 0
		for i := range es {
			e := &es[i]
			switch {
			case where == "front" && !e.cpe && i == 0:
				e.names = []string{"FC"}
			case where == "front" && e.cpe && pairs < 2:
				e.names = [][]string{{"FL", "FR"}, {"FLC", "FRC"}}[pairs]
				pairs++
			case where == "side" && e.cpe && pairs < 1:
				e.names = []string{"SL", "SR"}
				pairs++
			case where == "back" && e.cpe && pairs < 1 && singles == 0:
				e.names = []string{"BL", "BR"}
				pairs++
			case where == "back" && !e.cpe && singles < 1:
				e.names = []string{"BC"}
				singles++
			default:
				return unsupportedf("a program_config_element with %d front, %d side and %d back elements", numFront, numSide, numBack)
			}
		}
		return nil
	}
	if err := name("front", front); err != nil {
		return nil, err
	}
	if err := name("side", side); err != nil {
		return nil, err
	}
	if err := name("back", back); err != nil {
		return nil, err
	}
	if numLFE > 1 {
		return nil, unsupportedf("a program_config_element with %d LFE elements", numLFE)
	}
	for i := range lfe {
		lfe[i].names = []string{"LFE"}
	}
	pc.elems = append(append(append(append(pc.elems, front...), side...), back...), lfe...)
	for _, e := range pc.elems {
		pc.channels += len(e.names)
	}
	if pc.channels == 0 {
		return nil, fmt.Errorf("aac: a program_config_element with no channels")
	}
	return pc, nil
}

// programOrder puts a frame's channels (one or two for each element, in
// bitstream order, after SBR) in the program_config_element's order.
func (d *Decoder) programOrder(pcm [][]float64, elems []frameElement) ([][]float64, error) {
	if d.pce == nil || len(elems) == 1 {
		return pcm, nil
	}
	// Each element's first output channel.
	at := make([]int, len(elems))
	n := 0
	for i, e := range elems {
		at[i] = n
		n++
		if e.stereo {
			n++
		}
	}
	if n != len(pcm) {
		return nil, fmt.Errorf("aac: %d channels after SBR for %d elements", len(pcm), len(elems))
	}
	out := make([][]float64, 0, len(pcm))
	used := make([]bool, len(elems))
	for _, pe := range d.pce.elems {
		found := false
		for i, e := range elems {
			if used[i] || e.tag != pe.tag || e.stereo != pe.cpe || e.lfe != pe.lfe {
				continue
			}
			used[i], found = true, true
			out = append(out, pcm[at[i]:at[i]+len(pe.names)]...)
			break
		}
		if !found {
			return nil, fmt.Errorf("aac: the frame lacks the program's element (tag %d)", pe.tag)
		}
	}
	return out, nil
}

// ChannelNames are the decoded channels' speakers in output order, as
// ffmpeg names them, for channel configuration 0 (a program_config_element)
// and 3 to 7, 11 and 12 (ISO/IEC 14496-3 Table 1.19), and nil for mono and
// stereo output. A program of one SCE with parametric stereo is stereo
// output (#1754: ISO's PS conformance streams signal their mono core with a
// program_config_element), so nil, not the SCE's one name.
func (d *Decoder) ChannelNames() []string {
	if d.OutputChannels() <= 2 {
		return nil
	}
	if d.pce != nil {
		var names []string
		for _, e := range d.pce.elems {
			names = append(names, e.names...)
		}
		return names
	}
	if d.channels > 2 && d.channelConfig < len(configNames) {
		return configNames[d.channelConfig]
	}
	return nil
}

// configNames are the channels of configurations 3 to 7, 11 and 12 in
// bitstream order: the centre's SCE, the front CPE, then the side and back
// channels and the LFE. The second pair of configurations 7 and 12 is a
// side pair as ffmpeg 9 lays them out (7.1: its mono downmix weighs it ½,
// as checked against ffmpeg's decode of an AudioToolbox configuration 7
// stream, where a front-wide pair would be 0.12 off).
var configNames = [13][]string{
	3:  {"FC", "FL", "FR"},
	4:  {"FC", "FL", "FR", "BC"},
	5:  {"FC", "FL", "FR", "BL", "BR"},
	6:  {"FC", "FL", "FR", "BL", "BR", "LFE"},
	7:  {"FC", "FL", "FR", "SL", "SR", "BL", "BR", "LFE"},
	11: {"FC", "FL", "FR", "SL", "SR", "BC", "LFE"},
	12: {"FC", "FL", "FR", "SL", "SR", "BL", "BR", "LFE"},
}
