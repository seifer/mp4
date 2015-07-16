package filter

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/jfbus/mp4"
)

var (
	ErrClipOutside = errors.New("clip zone is outside video")
	// ErrTruncatedChunk  = errors.New("chunk was truncated")
	ErrInvalidDuration = errors.New("invalid duration")
)

type ErrorChunkTrunc struct {
	m      string
	i1, i2 int64
}

func (e *ErrorChunkTrunc) Error() string {
	return fmt.Sprintf("%s [%d != %d]", e.m, e.i1, e.i2)
}

type chunk struct {
	size      int64
	oldOffset int64
	newOffset int64
}

type trakInfo struct {
	rebuilded bool

	sci          int
	currentChunk int

	index         uint32
	startTC       uint32
	filterBegin   uint32
	filterEnd     uint32
	currentSample uint32
	firstSample   uint32
}

type clipFilter struct {
	firstChunk   int
	bufferLength int

	size    int64
	offset  int64
	forskip int64

	buffer []byte
	chunks []chunk

	m      *mp4.MP4
	reader io.Reader

	end   time.Duration
	begin time.Duration
}

type ClipInterface interface {
	Filter
	io.ReadSeeker
}

// Clip returns a filter that extracts a clip between begin and begin + duration (in seconds, starting at 0)
// Il will try to include a key frame at the beginning, and keeps the same chunks as the origin media
func Clip(m *mp4.MP4, begin, duration time.Duration) (ClipInterface, error) {
	end := begin + duration

	if begin < 0 {
		return nil, ErrClipOutside
	}

	if begin > m.Duration() {
		return nil, ErrClipOutside
	}

	if end > m.Duration() {
		end = m.Duration()
	}

	if end < 0 {
		return nil, ErrClipOutside
	}

	return &clipFilter{
		m:     m,
		end:   end,
		begin: begin,
	}, nil
}

func (f *clipFilter) Seek(offset int64, whence int) (int64, error) {
	noffset := f.offset

	if whence == os.SEEK_END {
		noffset = f.size + offset
	} else if whence == os.SEEK_SET {
		noffset = offset
	} else if whence == os.SEEK_CUR {
		noffset += offset
	} else {
		return -1, syscall.EINVAL
	}

	if noffset < 0 {
		return -1, syscall.EINVAL
	}

	if noffset > f.size {
		return -1, syscall.EINVAL
	}

	f.offset = noffset

	return noffset, nil
}

func (f *clipFilter) Read(buf []byte) (n int, err error) {
	var nn int

	if len(buf) == 0 {
		return
	}

	if int(f.offset) < f.bufferLength && len(buf) > 0 {
		nn := copy(buf, f.buffer[f.offset:])
		f.offset += int64(nn)
		n += nn
		buf = buf[nn:]

		if int(f.offset) >= f.bufferLength {
			f.buffer = nil
		}
	}

	s, seekable := f.reader.(io.ReadSeeker)

	for f.firstChunk < len(f.chunks) && err != nil && len(buf) > 0 {
		c := f.chunks[f.firstChunk]

		if f.offset >= c.newOffset+c.size {
			f.firstChunk++
			continue
		}

		realOffset := c.oldOffset + (f.offset - c.newOffset)
		if seekable {
			if _, err = s.Seek(realOffset, os.SEEK_SET); err != nil {
				return
			}
		}

		can := int(c.size - (f.offset - c.newOffset))

		if can > len(buf) {
			can = len(buf)
		}

		nn, err = io.ReadFull(f.reader, buf[:can])
		f.offset += int64(nn)
		n += nn
		buf = buf[nn:]

		if nn != can {
			if err == nil {
				err = &ErrorChunkTrunc{"chunk was truncated: Read", int64(can), int64(nn)}
			}
		}
	}

	return
}

func (f *clipFilter) Filter() (err error) {
	f.buildChunkList()

	bsz := uint32(mp4.BoxHeaderSize)
	bsz += uint32(f.m.Moov.Size())

	for _, b := range f.m.Boxes() {
		bsz += uint32(b.Size())
	}

	// Update chunk offset
	for _, t := range f.m.Moov.Trak {
		for i, _ := range t.Mdia.Minf.Stbl.Stco.ChunkOffset {
			t.Mdia.Minf.Stbl.Stco.ChunkOffset[i] += bsz
		}
	}

	// Prepare blob with moov and other small atoms
	buffer := make([]byte, 0)
	Buffer := bytes.NewBuffer(buffer)

	if err = f.m.Moov.Encode(Buffer); err != nil {
		return
	}

	for _, b := range f.m.Boxes() {
		if err = b.Encode(Buffer); err != nil {
			return
		}
	}

	buf := make([]byte, mp4.BoxHeaderSize)
	binary.BigEndian.PutUint32(buf, uint32(f.m.Mdat.Size()))
	copy(buf[4:], f.m.Mdat.Type())

	if _, err = Buffer.Write(buf); err != nil {
		return
	}

	f.size = int64(f.m.Size())
	f.buffer = Buffer.Bytes()
	f.reader = f.m.Mdat.Reader()
	f.bufferLength = len(f.buffer)

	f.compactChunks()

	f.m = nil

	return
}

func (f *clipFilter) compactChunks() {
	newChunks := make([]chunk, 0, 4)
	last := f.chunks[0]
	last.newOffset = int64(f.bufferLength)
	lastBound := last.oldOffset + last.size
	for i := 1; i < len(f.chunks); i++ {
		ch := f.chunks[i]
		if lastBound == ch.oldOffset {
			lastBound += ch.size
			last.size += ch.size
		} else {
			newChunks = append(newChunks, last)
			ch.newOffset = last.newOffset + last.size
			last = ch
			lastBound = ch.oldOffset + ch.size
		}
	}
	newChunks = append(newChunks, last)
	f.chunks = newChunks
}

func (f *clipFilter) WriteTo(w io.Writer) (n int64, err error) {
	var nn int
	var nnn int64

	if nn, err = w.Write(f.buffer); err != nil {
		return
	}

	n += int64(nn)
	s, seekable := f.reader.(io.Seeker)

	for _, c := range f.chunks {
		csize := int64(c.size)

		if seekable {
			if _, err = s.Seek(int64(c.oldOffset), os.SEEK_SET); err != nil {
				return
			}
		}

		nnn, err = io.CopyN(w, f.reader, csize)
		n += nnn
		if err != nil {
			return
		}

		if nnn != csize {
			if err == nil {
				err = &ErrorChunkTrunc{"chunk was truncated: WriteTo", csize, nnn}
			}
		}

		if err != nil {
			return
		}
	}

	return
}

func (f *clipFilter) WriteToN(dst io.Writer, size int64) (n int64, err error) {
	var nn int
	var nnn int64

	if size == 0 {
		return
	}

	for int(f.offset) < f.bufferLength && err == nil && n < size {
		can := int64(f.bufferLength - int(f.offset))

		if can > size {
			can = size
		}

		nn, err = dst.Write(f.buffer[f.offset : f.offset+can])
		f.offset += int64(nn)
		n += int64(nn)

		if int(f.offset) == f.bufferLength {
			f.buffer = nil
		}
	}

	s, seekable := f.reader.(io.ReadSeeker)

	for f.firstChunk < len(f.chunks) && err == nil && n < size {
		c := f.chunks[f.firstChunk]

		if f.offset >= c.newOffset+c.size {
			f.firstChunk++
			continue
		}

		realOffset := c.oldOffset + (f.offset - c.newOffset)

		if seekable {
			if _, err = s.Seek(realOffset, os.SEEK_SET); err != nil {
				return
			}
		}

		can := c.size - (f.offset - c.newOffset)

		if can > size-n {
			can = size - n
		}

		nnn, err = io.CopyN(dst, f.reader, can)
		f.offset += nnn
		n += nnn

		if nnn != can {
			if err == nil {
				err = &ErrorChunkTrunc{"chunk was truncated: WriteToN", can, nnn}
			}
		}
	}

	return
}

func (f *clipFilter) buildChunkList() {
	var sz, mt int
	var mv, off, size, firstTC, lastTC, sample, current, samples, descriptionID uint32

	for _, t := range f.m.Moov.Trak {
		sz += len(t.Mdia.Minf.Stbl.Stco.ChunkOffset)
	}

	f.m.Mdat.ContentSize = 0
	f.m.Moov.Mvhd.Duration = 0

	f.chunks = make([]chunk, 0, sz)

	cnt := len(f.m.Moov.Trak)
	ti := make([]trakInfo, cnt, cnt)

	newFirstChunk := make([][]uint32, cnt, cnt)
	newChunkOffset := make([][]uint32, cnt, cnt)
	newSamplesPerChunk := make([][]uint32, cnt, cnt)
	newSampleDescriptionID := make([][]uint32, cnt, cnt)

	firstChunkSamples := make([]uint32, cnt, cnt)
	firstChunkDescriptionID := make([]uint32, cnt, cnt)

	fbegin := f.begin
	fend := f.end

	// Find close l-frame from begin and end
	for tnum, t := range f.m.Moov.Trak {
		var p uint32

		cti := &ti[tnum]

		newFirstChunk[tnum] = make([]uint32, 0, len(t.Mdia.Minf.Stbl.Stsc.FirstChunk))
		newChunkOffset[tnum] = make([]uint32, 0, len(t.Mdia.Minf.Stbl.Stco.ChunkOffset))
		newSamplesPerChunk[tnum] = make([]uint32, 0, len(t.Mdia.Minf.Stbl.Stsc.SamplesPerChunk))
		newSampleDescriptionID[tnum] = make([]uint32, 0, len(t.Mdia.Minf.Stbl.Stsc.SampleDescriptionID))

		cti.filterBegin = uint32(int64(fbegin) * int64(t.Mdia.Mdhd.Timescale) / int64(time.Second))
		cti.filterEnd = uint32(int64(fend) * int64(t.Mdia.Mdhd.Timescale) / int64(time.Second))

		if stss := t.Mdia.Minf.Stbl.Stss; stss != nil {
			stts := t.Mdia.Minf.Stbl.Stts

			for i := 0; i < len(stss.SampleNumber); i++ {
				tc := stts.GetTimeCode(stss.SampleNumber[i] - 1)

				if tc > cti.filterBegin {
					cti.filterBegin = p
					fbegin = time.Second * time.Duration(p) / time.Duration(t.Mdia.Mdhd.Timescale)
					break
				}

				p = tc
			}
		}
	}

	// Skip excess chunks
	for tnum, t := range f.m.Moov.Trak {
		cti := &ti[tnum]

		stco := t.Mdia.Minf.Stbl.Stco
		stsc := t.Mdia.Minf.Stbl.Stsc
		stts := t.Mdia.Minf.Stbl.Stts

		for i, _ := range stco.ChunkOffset {
			if cti.sci < len(stsc.FirstChunk)-1 && i+1 >= int(stsc.FirstChunk[cti.sci+1]) {
				cti.sci++
			}

			samples = stsc.SamplesPerChunk[cti.sci]

			firstTC = stts.GetTimeCode(cti.currentSample)
			cti.currentSample += samples
			lastTC = stts.GetTimeCode(cti.currentSample)

			if lastTC < cti.filterBegin || firstTC > cti.filterEnd {
				continue
			}

			cti.startTC = firstTC
			cti.currentChunk = i
			cti.currentSample -= samples
			cti.firstSample = cti.currentSample + 1

			break
		}

		if cti.currentChunk == len(stco.ChunkOffset)-1 {
			cnt--
			cti.rebuilded = true
		}
	}

	for cnt > 0 {
		mv = 0

		for tnum, t := range f.m.Moov.Trak {
			if ti[tnum].rebuilded {
				continue
			}

			if mv == 0 || t.Mdia.Minf.Stbl.Stco.ChunkOffset[ti[tnum].currentChunk] < mv {
				mt = tnum
				mv = t.Mdia.Minf.Stbl.Stco.ChunkOffset[ti[tnum].currentChunk]
			}
		}

		cti := &ti[mt]
		newChunkOffset[mt] = append(newChunkOffset[mt], off)

		stsc := f.m.Moov.Trak[mt].Mdia.Minf.Stbl.Stsc
		stsz := f.m.Moov.Trak[mt].Mdia.Minf.Stbl.Stsz

		if cti.sci < len(stsc.FirstChunk)-1 && cti.currentChunk+1 >= int(stsc.FirstChunk[cti.sci+1]) {
			cti.sci++
		}

		samples := stsc.SamplesPerChunk[cti.sci]
		descriptionID = stsc.SampleDescriptionID[cti.sci]

		size = 0

		for i := 0; i < int(samples); i++ {
			cti.currentSample++
			size += stsz.GetSampleSize(int(cti.currentSample) - 1)
		}

		off += size
		f.m.Mdat.ContentSize += size

		f.chunks = append(f.chunks, chunk{
			size:      int64(size),
			oldOffset: int64(mv),
		})

		cti.index++

		if samples != firstChunkSamples[mt] || descriptionID != firstChunkDescriptionID[mt] {
			newFirstChunk[mt] = append(newFirstChunk[mt], cti.index)
			newSamplesPerChunk[mt] = append(newSamplesPerChunk[mt], samples)
			newSampleDescriptionID[mt] = append(newSampleDescriptionID[mt], descriptionID)
			firstChunkSamples[mt] = samples
			firstChunkDescriptionID[mt] = descriptionID
		}

		// Go in next chunk
		cti.currentChunk++

		if cti.currentChunk == len(f.m.Moov.Trak[mt].Mdia.Minf.Stbl.Stco.ChunkOffset) {
			cnt--
			cti.rebuilded = true
		}
	}

	for tnum, t := range f.m.Moov.Trak {
		cti := &ti[tnum]
		stts := t.Mdia.Minf.Stbl.Stts
		end := stts.GetTimeCode(cti.currentSample)

		t.Tkhd.Duration = ((end - cti.startTC) / t.Mdia.Mdhd.Timescale) * f.m.Moov.Mvhd.Timescale
		t.Mdia.Mdhd.Duration = end - cti.startTC

		if t.Tkhd.Duration > f.m.Moov.Mvhd.Duration {
			f.m.Moov.Mvhd.Duration = t.Tkhd.Duration
		}

		// stts - sample duration
		if stts := t.Mdia.Minf.Stbl.Stts; stts != nil {
			sample = 1
			current = 0

			firstSample := cti.firstSample
			currentSample := cti.currentSample

			oldSampleCount := stts.SampleCount
			oldSampleTimeDelta := stts.SampleTimeDelta

			newSampleCount := make([]uint32, 0, len(oldSampleCount))
			newSampleTimeDelta := make([]uint32, 0, len(oldSampleTimeDelta))

			for i := 0; i < len(oldSampleCount) && sample < currentSample; i++ {
				if sample+oldSampleCount[i] >= firstSample {
					switch {
					case sample <= firstSample && sample+oldSampleCount[i] > currentSample:
						current = currentSample - firstSample + 1
					case sample < firstSample:
						current = oldSampleCount[i] + sample - firstSample
					case sample+oldSampleCount[i] > currentSample:
						current = oldSampleCount[i] + sample - currentSample
					default:
						current = oldSampleCount[i]
					}

					newSampleCount = append(newSampleCount, current)
					newSampleTimeDelta = append(newSampleTimeDelta, oldSampleTimeDelta[i])
				}

				sample += oldSampleCount[i]
			}

			stts.SampleCount = newSampleCount
			stts.SampleTimeDelta = newSampleTimeDelta
		}

		// stss (key frames)
		if stss := t.Mdia.Minf.Stbl.Stss; stss != nil {
			firstSample := cti.firstSample
			currentSample := cti.currentSample

			oldSampleNumber := stss.SampleNumber
			newSampleNumber := make([]uint32, 0, len(oldSampleNumber))

			for _, n := range oldSampleNumber {
				if n >= firstSample && n <= currentSample {
					newSampleNumber = append(newSampleNumber, n-firstSample+1)
				}
			}

			stss.SampleNumber = newSampleNumber
		}

		// stsz (sample sizes)
		if stsz := t.Mdia.Minf.Stbl.Stsz; stsz != nil {
			stsz.SampleStart = cti.firstSample - 1
			stsz.SampleNumber = cti.currentSample - cti.firstSample + 1
		}

		// ctts - time offsets (b-frames)
		if ctts := t.Mdia.Minf.Stbl.Ctts; ctts != nil {
			sample = 1

			firstSample := cti.firstSample
			currentSample := cti.currentSample

			oldSampleCount := ctts.SampleCount
			oldSampleOffset := ctts.SampleOffset

			newSampleCount := make([]uint32, 0, len(oldSampleCount))
			newSampleOffset := make([]uint32, 0, len(oldSampleOffset))

			for i := 0; i < len(oldSampleCount) && sample < currentSample; i++ {
				if sample+oldSampleCount[i] >= firstSample {
					current := oldSampleCount[i]

					if sample+oldSampleCount[i] > firstSample && sample < firstSample {
						current += sample - firstSample
					}

					if sample+oldSampleCount[i] > currentSample {
						current += currentSample - sample - oldSampleCount[i]
					}

					newSampleCount = append(newSampleCount, current)
					newSampleOffset = append(newSampleOffset, oldSampleOffset[i])
				}

				sample += oldSampleCount[i]
			}

			ctts.SampleCount = newSampleCount
			ctts.SampleOffset = newSampleOffset
		}

		// co64 ?

		t.Mdia.Minf.Stbl.Stsc.FirstChunk = newFirstChunk[tnum]
		t.Mdia.Minf.Stbl.Stco.ChunkOffset = newChunkOffset[tnum]
		t.Mdia.Minf.Stbl.Stsc.SamplesPerChunk = newSamplesPerChunk[tnum]
		t.Mdia.Minf.Stbl.Stsc.SampleDescriptionID = newSampleDescriptionID[tnum]
	}
}
