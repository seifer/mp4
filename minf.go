package mp4

import "io"

// Media Information Box (minf - mandatory)
//
// Contained in : Media Box (mdia)
//
// Status: partially decoded (hmhd - hint tracks - and nmhd - null media - are ignored)
type MinfBox struct {
	Stbl  *StblBox
	boxes []Box
}

func DecodeMinf(r io.Reader) (Box, error) {
	l, err := DecodeContainer(r)
	if err != nil {
		return nil, err
	}
	m := &MinfBox{
		boxes: make([]Box, 0, len(l)),
	}
	for _, b := range l {
		switch b.Type() {
		case "stbl":
			m.Stbl = b.(*StblBox)
		default:
			m.boxes = append(m.boxes, b)
		}
	}
	return m, nil
}

func (b *MinfBox) Type() string {
	return "minf"
}

func (b *MinfBox) Size() (sz int) {
	sz += b.Stbl.Size()

	for _, box := range b.boxes {
		sz += box.Size()
	}

	return sz + BoxHeaderSize
}

func (b *MinfBox) Dump() {
	b.Stbl.Dump()
}

func (b *MinfBox) Encode(w io.Writer) (err error) {
	if err = EncodeHeader(b, w); err != nil {
		return
	}

	for _, b := range b.boxes {
		if err = b.Encode(w); err != nil {
			return
		}
	}

	return b.Stbl.Encode(w)
}
