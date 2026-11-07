// Package qr draws QR codes (ISO/IEC 18004): byte mode, error correction
// level M, versions 1 to 10 -- up to 213 bytes, which holds an authenticator
// enrolment URI with room to spare. Written here rather than imported: it is
// small, and what it draws is a secret.
package qr

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a square of modules; true is dark.
type Code struct {
	Size    int
	Version int
	Mask    int
	dark    [][]bool
}

// Dark reports whether module (x, y) is dark.
func (c *Code) Dark(x, y int) bool { return c.dark[y][x] }

// ErrTooLong is returned for more than version 10 holds.
var ErrTooLong = errors.New("qr: text too long for version 10, level M")

// level M blocks per version: error-correction codewords per block, and the
// (count, data codewords) of each group.
type blockSpec struct {
	ec     int
	groups [][2]int
}

var specM = [11]blockSpec{
	{},
	{10, [][2]int{{1, 16}}},
	{16, [][2]int{{1, 28}}},
	{26, [][2]int{{1, 44}}},
	{18, [][2]int{{2, 32}}},
	{24, [][2]int{{2, 43}}},
	{16, [][2]int{{4, 27}}},
	{18, [][2]int{{4, 31}}},
	{22, [][2]int{{2, 38}, {2, 39}}},
	{22, [][2]int{{3, 36}, {2, 37}}},
	{26, [][2]int{{4, 43}, {1, 44}}},
}

var alignment = [11][]int{nil, nil, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34},
	{6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50}}

var remainderBits = [11]int{0, 0, 7, 7, 7, 7, 7, 0, 0, 0, 0}

func dataCodewords(v int) int {
	n := 0
	for _, g := range specM[v].groups {
		n += g[0] * g[1]
	}
	return n
}

// Encode draws text in the smallest version that holds it.
func Encode(text string) (*Code, error) {
	data := []byte(text)
	for v := 1; v <= 10; v++ {
		countBits := 8
		if v >= 10 {
			countBits = 16
		}
		if 4+countBits+8*len(data) <= 8*dataCodewords(v) {
			return build(v, data, countBits), nil
		}
	}
	return nil, ErrTooLong
}

type bitBuf struct {
	b []byte
	n int
}

func (w *bitBuf) put(v uint, bits int) {
	for i := bits - 1; i >= 0; i-- {
		if w.n%8 == 0 {
			w.b = append(w.b, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.b[w.n/8] |= 0x80 >> uint(w.n%8)
		}
		w.n++
	}
}

func build(v int, data []byte, countBits int) *Code {
	capacity := dataCodewords(v)
	w := &bitBuf{}
	w.put(0b0100, 4) // byte mode
	w.put(uint(len(data)), countBits)
	for _, b := range data {
		w.put(uint(b), 8)
	}
	// Terminator, then pad to a byte, then the alternating pad codewords.
	term := 8*capacity - w.n
	if term > 4 {
		term = 4
	}
	w.put(0, term)
	if w.n%8 != 0 {
		w.put(0, 8-w.n%8)
	}
	for i := 0; len(w.b) < capacity; i++ {
		w.b = append(w.b, [2]byte{0xEC, 0x11}[i%2])
	}

	// Split into blocks, add error correction, interleave.
	spec := specM[v]
	var blocks [][]byte
	off := 0
	for _, g := range spec.groups {
		for i := 0; i < g[0]; i++ {
			blocks = append(blocks, w.b[off:off+g[1]])
			off += g[1]
		}
	}
	gen := generator(spec.ec)
	var ecs [][]byte
	for _, b := range blocks {
		ecs = append(ecs, rsRemainder(b, gen))
	}
	var out []byte
	for i := 0; ; i++ {
		any := false
		for _, b := range blocks {
			if i < len(b) {
				out = append(out, b[i])
				any = true
			}
		}
		if !any {
			break
		}
	}
	for i := 0; i < spec.ec; i++ {
		for _, e := range ecs {
			out = append(out, e[i])
		}
	}

	best, bestPenalty := (*Code)(nil), 0
	for mask := 0; mask < 8; mask++ {
		c := place(v, out, mask)
		if p := penalty(c); best == nil || p < bestPenalty {
			best, bestPenalty = c, p
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// Reed-Solomon over GF(256), polynomial 0x11D
// ---------------------------------------------------------------------------

var expT, logT [256]byte

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		expT[i] = byte(x)
		logT[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
}

func gmul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return expT[(int(logT[a])+int(logT[b]))%255]
}

// generator returns the degree-n generator polynomial, highest term first
// (the leading 1 omitted).
func generator(n int) []byte {
	g := []byte{1}
	for i := 0; i < n; i++ {
		next := make([]byte, len(g)+1)
		for j, c := range g {
			next[j] ^= c
			next[j+1] ^= gmul(c, expT[i])
		}
		g = next
	}
	return g[1:]
}

func rsRemainder(data, gen []byte) []byte {
	rem := make([]byte, len(gen))
	for _, d := range data {
		f := d ^ rem[0]
		copy(rem, rem[1:])
		rem[len(rem)-1] = 0
		for i, g := range gen {
			rem[i] ^= gmul(g, f)
		}
	}
	return rem
}

// ---------------------------------------------------------------------------
// Placement
// ---------------------------------------------------------------------------

func place(v int, codewords []byte, mask int) *Code {
	size := 17 + 4*v
	c := &Code{Size: size, Version: v, Mask: mask, dark: make([][]bool, size)}
	reserved := make([][]bool, size)
	for i := range c.dark {
		c.dark[i] = make([]bool, size)
		reserved[i] = make([]bool, size)
	}
	set := func(x, y int, d bool) {
		if x >= 0 && y >= 0 && x < size && y < size {
			c.dark[y][x] = d
			reserved[y][x] = true
		}
	}
	finder := func(cx, cy int) {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				m := max(abs(dx), abs(dy))
				set(cx+dx, cy+dy, m != 2 && m != 4)
			}
		}
	}
	finder(3, 3)
	finder(size-4, 3)
	finder(3, size-4)
	for i := 8; i < size-8; i++ {
		set(i, 6, i%2 == 0)
		set(6, i, i%2 == 0)
	}
	for _, ay := range alignment[v] {
		for _, ax := range alignment[v] {
			if reserved[ay][ax] {
				continue // overlaps a finder
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					set(ax+dx, ay+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	set(8, size-8, true) // the dark module
	// Format and version areas: reserved now, written once the mask is known.
	for i := 0; i < 9; i++ {
		reserved[8][i], reserved[i][8] = true, true
	}
	for i := 0; i < 8; i++ {
		reserved[8][size-1-i], reserved[size-1-i][8] = true, true
	}
	if v >= 7 {
		for i := 0; i < 6; i++ {
			for j := 0; j < 3; j++ {
				reserved[i][size-11+j], reserved[size-11+j][i] = true, true
			}
		}
	}

	// Data, in two-module columns from the right, zig-zagging.
	bit := 0
	total := len(codewords)*8 + remainderBits[v]
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5 // the vertical timing column is skipped
		}
		for vert := 0; vert < size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := (right+1)&2 == 0
				y := vert
				if upward {
					y = size - 1 - vert
				}
				if reserved[y][x] {
					continue
				}
				d := false
				if bit < len(codewords)*8 {
					d = codewords[bit/8]>>(7-uint(bit%8))&1 == 1
				}
				if bit < total {
					bit++
				}
				if masked(mask, x, y) {
					d = !d
				}
				c.dark[y][x] = d
			}
		}
	}

	// Format information: level M (00) and the mask, BCH(15,5), XOR 0x5412.
	format := formatBits(mask)
	for i := 0; i < 15; i++ {
		d := format>>uint(i)&1 == 1
		// Around the top-left finder.
		switch {
		case i < 6:
			c.dark[i][8] = d
		case i < 8:
			c.dark[i+1][8] = d
		default:
			c.dark[8][14-i+boolInt(i == 8)] = d
		}
		// The split copy by the other two finders.
		if i < 8 {
			c.dark[8][size-1-i] = d
		} else {
			c.dark[size-15+i][8] = d
		}
	}
	if v >= 7 {
		vb := versionBits(v)
		for i := 0; i < 18; i++ {
			d := vb>>uint(i)&1 == 1
			c.dark[i/3][size-11+i%3] = d
			c.dark[size-11+i%3][i/3] = d
		}
	}
	return c
}

func masked(mask, x, y int) bool {
	switch mask {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (y/2+x/3)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	default:
		return ((x+y)%2+x*y%3)%2 == 0
	}
}

func formatBits(mask int) int {
	data := 0b00<<3 | mask // level M
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	return (data<<10 | rem) ^ 0x5412
}

func versionBits(v int) int {
	rem := v
	for i := 0; i < 12; i++ {
		rem = rem<<1 ^ (rem>>11)*0x1F25
	}
	return v<<12 | rem
}

// ---------------------------------------------------------------------------
// Mask penalty (the four rules of the standard)
// ---------------------------------------------------------------------------

func penalty(c *Code) int {
	n, p := c.Size, 0
	at := func(x, y int, col bool) bool {
		if col {
			return c.dark[x][y]
		}
		return c.dark[y][x]
	}
	for _, col := range []bool{false, true} {
		for y := 0; y < n; y++ {
			run := 1
			for x := 1; x < n; x++ {
				if at(x, y, col) == at(x-1, y, col) {
					run++
					if run == 5 {
						p += 3
					} else if run > 5 {
						p++
					}
				} else {
					run = 1
				}
			}
			for x := 0; x+10 < n; x++ { // 1:1:3:1:1 with four light either side
				pat := []bool{true, false, true, true, true, false, true, false, false, false, false}
				f, b := true, true
				for k := 0; k < 11; k++ {
					f = f && at(x+k, y, col) == pat[k]
					b = b && at(x+k, y, col) == pat[10-k]
				}
				if f {
					p += 40
				}
				if b {
					p += 40
				}
			}
		}
	}
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if c.dark[y][x] {
				dark++
			}
			if x+1 < n && y+1 < n && c.dark[y][x] == c.dark[y][x+1] && c.dark[y][x] == c.dark[y+1][x] && c.dark[y][x] == c.dark[y+1][x+1] {
				p += 3
			}
		}
	}
	p += abs(dark*20-n*n*10) / (n * n) * 10
	return p
}

// SVG draws the code with a four-module quiet zone, one path, no script.
func (c *Code) SVG() string {
	q := c.Size + 8
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`+
		`<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, q, q, q, q)
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.dark[y][x] {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+4, y+4)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
