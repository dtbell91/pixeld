package panel

import "sort"

// Decode reads a framebuffer half back into the picture the panel shows: each LED's three 16-bit words
// mapped back through the lookup tables (nearest entry). Brightness scaling is baked in, so a frame shown
// at 50 % decodes to roughly half the values.
func Decode(half []uint32) Frame {
	var f Frame
	for y := 0; y < Size; y++ {
		for x := 0; x < Size; x++ {
			slots := half[slotBase[y][x] : slotBase[y][x]+16]
			var w [3]uint16
			for i, v := range slots {
				bit := uint(15 - i)
				w[0] |= uint16(v>>21&1) << bit
				w[1] |= uint16(v>>13&1) << bit
				w[2] |= uint16(v>>5&1) << bit
			}
			for c := 0; c < 3; c++ {
				f[y][x][c] = inverse(c, w[c])
			}
		}
	}
	return f
}

func inverse(c int, word uint16) uint8 {
	t := gamma[c][:]
	i := sort.Search(len(t), func(i int) bool { return t[i] >= word })
	if i == len(t) {
		return 255
	}
	if i > 0 && word-t[i-1] < t[i]-word {
		i--
	}
	return uint8(i)
}
