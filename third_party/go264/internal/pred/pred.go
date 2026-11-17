package pred

type Availability uint8

const (
	AvailLeft Availability = 1 << iota
	AvailTop
	AvailTopLeft
	AvailTopRight
)

func (a Availability) Has(f Availability) bool {
	return a&f == f
}

const (
	I4x4Vertical = iota
	I4x4Horizontal
	I4x4DC
	I4x4DiagonalDownLeft
	I4x4DiagonalDownRight
	I4x4VerticalRight
	I4x4HorizontalDown
	I4x4VerticalLeft
	I4x4HorizontalUp
)

const (
	I16x16Vertical = iota
	I16x16Horizontal
	I16x16DC
	I16x16Plane
)

const (
	ChromaDC = iota
	ChromaHorizontal
	ChromaVertical
	ChromaPlane
)

func clip1(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func Intra4x4ModeAvailable(mode int, avail Availability) bool {
	switch mode {
	case I4x4Vertical:
		return avail.Has(AvailTop)
	case I4x4Horizontal:
		return avail.Has(AvailLeft)
	case I4x4DC:
		return true
	case I4x4DiagonalDownLeft:
		return avail.Has(AvailTop)
	case I4x4DiagonalDownRight:
		return avail.Has(AvailLeft) && avail.Has(AvailTop) && avail.Has(AvailTopLeft)
	case I4x4VerticalRight:
		return avail.Has(AvailLeft) && avail.Has(AvailTop) && avail.Has(AvailTopLeft)
	case I4x4HorizontalDown:
		return avail.Has(AvailLeft) && avail.Has(AvailTop) && avail.Has(AvailTopLeft)
	case I4x4VerticalLeft:
		return avail.Has(AvailTop)
	case I4x4HorizontalUp:
		return avail.Has(AvailLeft)
	default:
		return false
	}
}

func Intra16x16ModeAvailable(mode int, avail Availability) bool {
	switch mode {
	case I16x16Vertical:
		return avail.Has(AvailTop)
	case I16x16Horizontal:
		return avail.Has(AvailLeft)
	case I16x16DC:
		return true
	case I16x16Plane:
		return avail.Has(AvailLeft) && avail.Has(AvailTop) && avail.Has(AvailTopLeft)
	default:
		return false
	}
}

func ChromaModeAvailable(mode int, avail Availability) bool {
	switch mode {
	case ChromaDC:
		return true
	case ChromaHorizontal:
		return avail.Has(AvailLeft)
	case ChromaVertical:
		return avail.Has(AvailTop)
	case ChromaPlane:
		return avail.Has(AvailLeft) && avail.Has(AvailTop) && avail.Has(AvailTopLeft)
	default:
		return false
	}
}
