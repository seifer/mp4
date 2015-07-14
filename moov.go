package mp4

import (
	"fmt"
	"io"
)

// Movie Box (moov - mandatory)
//
// Status: partially decoded (anything other than mvhd, iods, trak or udta is ignored)
//
// Contains all meta-data. To be able to stream a file, the moov box should be placed before the mdat box.
type MoovBox struct {
	Mvhd  *MvhdBox
	Trak  []*TrakBox
	boxes []Box
}

func DecodeMoov(r io.Reader) (Box, error) {
	l, err := DecodeContainer(r)
	if err != nil {
		return nil, err
	}
	m := &MoovBox{}
	for _, b := range l {
		switch b.Type() {
		case "mvhd":
			m.Mvhd = b.(*MvhdBox)
		case "trak":
			m.Trak = append(m.Trak, b.(*TrakBox))
		default:
			m.boxes = append(m.boxes, b)
		}
	}
	return m, nil
}

func (b *MoovBox) Type() string {
	return "moov"
}

func (b *MoovBox) Size() (sz int) {
	sz += b.Mvhd.Size()

	for _, t := range b.Trak {
		sz += t.Size()
	}

	for _, box := range b.boxes {
		sz += box.Size()
	}

	return sz + BoxHeaderSize
}

func (b *MoovBox) Dump() {
	b.Mvhd.Dump()
	for i, t := range b.Trak {
		fmt.Println("Track", i)
		t.Dump()
	}
}

func (b *MoovBox) Encode(w io.Writer) (err error) {
	if err = EncodeHeader(b, w); err != nil {
		return
	}

	for _, t := range b.Trak {
		if err = t.Encode(w); err != nil {
			return
		}
	}

	for _, b := range b.boxes {
		if err = b.Encode(w); err != nil {
			return
		}
	}

	return b.Mvhd.Encode(w)
}
