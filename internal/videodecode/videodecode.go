// Package videodecode decodes the AVI, FLV, MPEG and WMV video
// multimodalembedding@001 takes (#1651) with FFmpeg, run in a pinned
// container (this package's Dockerfile: Debian's ffmpeg package on the
// official debian image), `docker run --network none --read-only`. FFmpeg
// is LGPL/GPL, so the CLI links none of it and the repository holds none
// of its code: it runs as a separate program, as the TensorFlow converter
// image does (internal/tfconvert). ffprobe lists a video's frames, and
// ffmpeg decodes the sampled ones to their native raw planes, which
// internal/embedding converts to RGB exactly as it converts the frames of
// the video it decodes itself.
package videodecode

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/embedding"
)

// Dockerfile is the decoder image's build context.
//
//go:embed Dockerfile
var Dockerfile []byte

// Tag is the image's tag: the Dockerfile's hash, so a changed Dockerfile
// is a new image, never a stale one.
func Tag() string {
	h := sha256.Sum256(Dockerfile)
	return "cloudburrow/videodecode:" + hex.EncodeToString(h[:])[:16]
}

// BuildTimeout bounds one build of the image (pulling Debian and its
// ffmpeg package, about 250 MB); it is not a request's deadline.
const BuildTimeout = 20 * time.Minute

// runTimeout bounds one ffprobe or ffmpeg run.
const runTimeout = 5 * time.Minute

// Docker runs the decoder image with the docker CLI.
type Docker struct {
	// Image is the image's tag ("": Tag()), built from Dockerfile when
	// it is missing.
	Image string
	// Docker is the docker binary ("": docker on PATH).
	Docker string
	// TempDir holds each request's video while it is decoded ("": the
	// system's temporary directory); docker must be able to mount it.
	TempDir string
	// Logf, when set, is told when a build starts and how it ended.
	Logf func(format string, args ...any)

	mu    sync.Mutex
	built bool
	build *imageBuild
}

type imageBuild struct {
	done chan struct{}
	err  error
}

func (d *Docker) image() string {
	if d.Image != "" {
		return d.Image
	}
	return Tag()
}

func (d *Docker) bin() string {
	if d.Docker != "" {
		return d.Docker
	}
	return "docker"
}

func (d *Docker) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

// unavailable is an error that matches embedding.ErrDecoderUnavailable.
type unavailable struct{ msg string }

func (e *unavailable) Error() string        { return e.msg }
func (e *unavailable) Is(target error) bool { return target == embedding.ErrDecoderUnavailable }

// Build builds the image when it is missing and waits for it, as long as
// ctx allows.
func (d *Docker) Build(ctx context.Context) error {
	d.mu.Lock()
	if d.built {
		d.mu.Unlock()
		return nil
	}
	b := d.build
	if b == nil {
		b = &imageBuild{done: make(chan struct{})}
		d.build = b
		go func() {
			b.err = d.buildImage()
			d.mu.Lock()
			d.built, d.build = b.err == nil, nil
			d.mu.Unlock()
			close(b.done)
		}()
	}
	d.mu.Unlock()
	select {
	case <-b.done:
		return b.err
	case <-ctx.Done():
		return &unavailable{"the video decoder image " + d.image() +
			" is still being built (its first use installs FFmpeg); it goes on building, try again shortly: " + ctx.Err().Error()}
	}
}

func (d *Docker) buildImage() error {
	ctx, cancel := context.WithTimeout(context.Background(), BuildTimeout)
	defer cancel()
	if _, err := exec.LookPath(d.bin()); err != nil {
		return &unavailable{"decoding AVI, FLV, MPEG and WMV video needs Docker, which is not installed: " + err.Error()}
	}
	if exec.CommandContext(ctx, d.bin(), "image", "inspect", d.image()).Run() == nil {
		return nil
	}
	d.logf("video decoder: building %s (FFmpeg, once)", d.image())
	began := time.Now()
	cmd := exec.CommandContext(ctx, d.bin(), "build", "-t", d.image(), "-")
	cmd.Stdin = bytes.NewReader(Dockerfile)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		d.logf("video decoder: building %s failed after %s: %v", d.image(), time.Since(began).Round(time.Second), err)
		return &unavailable{fmt.Sprintf("could not build the video decoder image %s: %v: %s", d.image(), err, tail(out.String()))}
	}
	d.logf("video decoder: built %s in %s", d.image(), time.Since(began).Round(time.Second))
	return nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return s
}

// Source is a ContainerDecoder for one request: it decodes with d under
// ctx, writing the video to a file once and probing it once. Close removes
// the file.
func (d *Docker) Source(ctx context.Context) *Source { return &Source{d: d, ctx: ctx} }

// Source decodes one request's video.
type Source struct {
	d   *Docker
	ctx context.Context

	dir    string
	key    *byte
	n      int
	probe  *probe
	probed error
}

type probe struct {
	video  embedding.ProbedVideo
	w, h   int
	format string // the raw format asked of ffmpeg
	matrix int
	full   bool
}

// Close removes the request's file.
func (s *Source) Close() error {
	if s.dir == "" {
		return nil
	}
	return os.RemoveAll(s.dir)
}

// file writes data to the request's file, once.
func (s *Source) file(data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("the video data is empty")
	}
	if s.dir != "" && s.key == &data[0] && s.n == len(data) {
		return filepath.Join(s.dir, "video"), nil
	}
	if s.dir == "" {
		dir, err := os.MkdirTemp(s.d.TempDir, "cb-videodecode-")
		if err != nil {
			return "", err
		}
		// The image runs as nobody, which must read the file.
		if err := os.Chmod(dir, 0o755); err != nil {
			return "", err
		}
		s.dir = dir
	}
	p := filepath.Join(s.dir, "video")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return "", err
	}
	s.key, s.n, s.probe, s.probed = &data[0], len(data), nil, nil
	return p, nil
}

// run runs one command in the image on the file, returning its stdout,
// or streaming it to out when out is set.
func (s *Source) run(file string, out io.Writer, args ...string) ([]byte, error) {
	if err := s.d.Build(s.ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(s.ctx, runTimeout)
	defer cancel()
	full := append([]string{"run", "--rm", "--network", "none", "--read-only", "--user", "65534:65534",
		"-v", file + ":/in/video:ro", s.d.image()}, args...)
	cmd := exec.CommandContext(ctx, s.d.bin(), full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if out != nil {
		cmd.Stdout = out
	}
	if err := cmd.Run(); err != nil {
		if s.ctx.Err() != nil {
			return nil, s.ctx.Err()
		}
		var ee *exec.ExitError
		msg := tail(stderr.String())
		if errors.As(err, &ee) && ee.ExitCode() == 125 {
			// docker itself failed (the daemon is down...).
			return nil, &unavailable{"the video decoder could not run: " + msg}
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("the data is not a video FFmpeg decodes: %s", msg)
	}
	return stdout.Bytes(), nil
}

// ffprobe's JSON.
type probeJSON struct {
	Streams []struct {
		CodecName  string `json:"codec_name"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		PixFmt     string `json:"pix_fmt"`
		ColorRange string `json:"color_range"`
		ColorSpace string `json:"color_space"`
	} `json:"streams"`
	Frames []struct {
		Timestamp string `json:"best_effort_timestamp_time"`
		Duration  string `json:"duration_time"`
	} `json:"frames"`
}

// matrices maps FFmpeg's colour space names to ISO/IEC 23091-2
// matrix_coefficients.
var matrices = map[string]int{"gbr": 0, "bt709": 1, "fcc": 4, "bt470bg": 5, "smpte170m": 6, "smpte240m": 7,
	"ycgco": 8, "bt2020nc": 9, "bt2020c": 10}

// ProbeFrames lists the first video stream's frames as ffprobe decodes
// them, in presentation order.
func (s *Source) ProbeFrames(data []byte) (*embedding.ProbedVideo, error) {
	file, err := s.file(data)
	if err != nil {
		return nil, err
	}
	if s.probe == nil && s.probed == nil {
		s.probe, s.probed = s.runProbe(file)
	}
	if s.probed != nil {
		return nil, s.probed
	}
	v := s.probe.video
	return &v, nil
}

func (s *Source) runProbe(file string) (*probe, error) {
	out, err := s.run(file, nil, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,width,height,pix_fmt,color_range,color_space:frame=best_effort_timestamp_time,duration_time",
		"-of", "json", "/in/video")
	if err != nil {
		return nil, err
	}
	var pj probeJSON
	if err := json.Unmarshal(out, &pj); err != nil {
		return nil, fmt.Errorf("reading ffprobe's output: %v", err)
	}
	if len(pj.Streams) == 0 {
		return nil, fmt.Errorf("the data has no video stream")
	}
	st := pj.Streams[0]
	p := &probe{video: embedding.ProbedVideo{Codec: st.CodecName}, w: st.Width, h: st.Height,
		format: st.PixFmt, matrix: 2, full: st.ColorRange == "pc"}
	if mc, ok := matrices[st.ColorSpace]; ok {
		p.matrix = mc
	}
	if !embedding.RawFormat(p.format) {
		p.format = "rgb24"
	}
	if p.w <= 0 || p.h <= 0 {
		return nil, fmt.Errorf("the video stream is %dx%d", p.w, p.h)
	}
	if len(pj.Frames) == 0 {
		return nil, fmt.Errorf("the video stream has no frames")
	}
	ts, durs := make([]float64, len(pj.Frames)), make([]float64, len(pj.Frames))
	for i, f := range pj.Frames {
		t, err := strconv.ParseFloat(f.Timestamp, 64)
		if err != nil {
			t = math.NaN() // a frame the decoder flushed with no timestamp
		}
		d, err := strconv.ParseFloat(f.Duration, 64)
		if err != nil {
			d = math.NaN()
		}
		ts[i], durs[i] = t, d
	}
	p.video.Times, p.video.End = frameTimes(ts, durs)
	return p, nil
}

// frameTimes are the presentation times of frames whose decoder output
// timestamps are ts (NaN where it gave none) and durations durs (NaN where
// it gave none), from the first frame's, and the last frame's end.
//
// A frame with no timestamp (the last B-frames an AVI or MPEG program
// stream flushes) follows the one before it by the stream's frame
// interval, the median gap between frames; the last frame lasts its own
// duration when the container gives a plausible one (within half to twice
// the interval: an AVI states its time base, not its frames' durations),
// the interval otherwise, so the video ends where its MP4 would.
func frameTimes(ts, durs []float64) ([]time.Duration, time.Duration) {
	var gaps []float64
	for i := 1; i < len(ts); i++ {
		if g := ts[i] - ts[i-1]; g > 0 {
			gaps = append(gaps, g)
		}
	}
	interval := 0.0
	if len(gaps) > 0 {
		sort.Float64s(gaps)
		interval = gaps[len(gaps)/2]
	}
	if interval == 0 {
		for _, d := range durs {
			if d > 0 {
				interval = d
				break
			}
		}
	}
	first := math.NaN()
	for i := range ts {
		if math.IsNaN(ts[i]) {
			if i == 0 {
				ts[i] = 0
			} else {
				ts[i] = ts[i-1] + interval
			}
		}
		if math.IsNaN(first) || ts[i] < first {
			first = ts[i]
		}
	}
	last := len(ts) - 1
	d := durs[last]
	if math.IsNaN(d) || d <= 0 || (interval > 0 && (d < interval/2 || d > 2*interval)) {
		d = interval
	}
	end := 0.0
	for _, t := range ts {
		end = math.Max(end, t)
	}
	end += d
	times := make([]time.Duration, len(ts))
	for i, t := range ts {
		times[i] = seconds(t - first)
	}
	return times, seconds(end - first)
}

func seconds(f float64) time.Duration { return time.Duration(math.Round(f * float64(time.Second))) }

// DecodeFrames decodes the frames at presentation indexes idx with ffmpeg,
// in the stream's native planes.
func (s *Source) DecodeFrames(data []byte, idx []int, yield func(int, embedding.RawFrame) error) error {
	if len(idx) == 0 {
		return nil
	}
	if _, err := s.ProbeFrames(data); err != nil {
		return err
	}
	file, err := s.file(data)
	if err != nil {
		return err
	}
	p := s.probe
	terms := make([]string, len(idx))
	for i, n := range idx {
		terms[i] = "eq(n\\," + strconv.Itoa(n) + ")"
	}
	size := embedding.RawFrameSize(p.format, p.w, p.h)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		defer pr.Close()
		buf := make([]byte, size)
		for k := 0; ; k++ {
			if _, err := io.ReadFull(pr, buf); err != nil {
				if err == io.EOF && k == len(idx) {
					done <- nil
				} else if err == io.EOF || err == io.ErrUnexpectedEOF {
					done <- fmt.Errorf("ffmpeg decoded %d of the video's %d sampled frames", k, len(idx))
				} else {
					done <- err
				}
				return
			}
			if k >= len(idx) {
				done <- fmt.Errorf("ffmpeg decoded more frames than the %d sampled", len(idx))
				return
			}
			f := embedding.RawFrame{W: p.w, H: p.h, Format: p.format, Pix: append([]byte(nil), buf...), Matrix: p.matrix, FullRange: p.full}
			if err := yield(idx[k], f); err != nil {
				done <- err
				return
			}
		}
	}()
	_, err = s.run(file, pw, "ffmpeg", "-v", "error", "-nostdin", "-noautorotate", "-i", "/in/video",
		"-map", "0:v:0", "-an", "-sn", "-dn", "-vf", "select='"+strings.Join(terms, "+")+"'",
		"-fps_mode", "passthrough", "-f", "rawvideo", "-pix_fmt", p.format, "pipe:1")
	pw.CloseWithError(err)
	rerr := <-done
	if err != nil {
		return err
	}
	return rerr
}
