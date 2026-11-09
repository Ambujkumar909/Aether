package qr

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Texts that land in each version 1 to 10 at level M (byte mode: 14, 26,
// 42, 62, 84, 106, 122, 152, 180, 213 bytes), plus the real payload.
func samples() map[string]string {
	out := map[string]string{
		"otpauth": "otpauth://totp/Aether:zoe%40example.com?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=Aether&algorithm=SHA1&digits=6&period=30",
		"short":   "hi",
		"utf8":    "Zoë — Aether ✓",
	}
	for v, n := range map[int]int{1: 14, 2: 26, 3: 42, 4: 62, 5: 84, 6: 106, 7: 122, 8: 152, 9: 180, 10: 213} {
		s := strings.Repeat("v", 0)
		for len(s) < n {
			s += fmt.Sprintf("%d:", v)
		}
		out[fmt.Sprintf("v%02d", v)] = s[:n]
	}
	return out
}

// PNG renders the code at 8 px per module with the quiet zone, for a
// decoder to read.
func toPNG(c *Code, path string) error {
	const px, quiet = 8, 4
	n := (c.Size + 2*quiet) * px
	img := image.NewGray(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			img.SetGray(x, y, color.Gray{Y: 255})
		}
	}
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if !c.Dark(x, y) {
				continue
			}
			for dy := 0; dy < px; dy++ {
				for dx := 0; dx < px; dx++ {
					img.SetGray((x+quiet)*px+dx, (y+quiet)*px+dy, color.Gray{Y: 0})
				}
			}
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return err
	}
	// And a PGM beside it: zbar's image reader in a bare container has no
	// PNG support, but reads PNM natively.
	pgm, err := os.Create(strings.TrimSuffix(path, ".png") + ".pgm")
	if err != nil {
		return err
	}
	defer pgm.Close()
	fmt.Fprintf(pgm, "P5 %d %d 255\n", n, n)
	_, err = pgm.Write(img.Pix)
	return err
}

// refCode reads a qrencode "-t ASCIIi" dump: one row per module, two
// characters per module. The "i" is for inverted: dark modules are the
// spaces, light ones "##".
func refCode(text string) (*Code, bool) {
	rows := strings.Split(strings.TrimRight(text, "\n"), "\n")
	n := len(rows)
	if n < 21 {
		return nil, false
	}
	c := &Code{Size: n, dark: make([][]bool, n)}
	for y, r := range rows {
		if len(r) < 2*n {
			return nil, false
		}
		c.dark[y] = make([]bool, n)
		for x := 0; x < n; x++ {
			c.dark[y][x] = r[2*x] != '#'
		}
	}
	return c, true
}

// With QR_PNG_DIR holding qrencode's matrices (<name>.ref, made with
// "-8 -l M"), every module must match once the mask is the same. Mask choice
// is a tie-break both encoders may make differently; data, error correction
// and placement may not differ at all.
func TestMatchesTheReferenceEncoderModuleForModule(t *testing.T) {
	dir := os.Getenv("QR_PNG_DIR")
	if dir == "" {
		t.Skip("QR_PNG_DIR not set")
	}
	compared := 0
	for name, text := range samples() {
		raw, err := os.ReadFile(filepath.Join(dir, name+".ref"))
		if err != nil {
			continue
		}
		ref, ok := refCode(string(raw))
		if !ok {
			t.Fatalf("%s: unreadable reference", name)
		}
		_, mask, ok := readFormat(ref)
		if !ok {
			t.Errorf("%s: the reference's format bits do not decode as level M (size %d)", name, ref.Size)
			continue
		}
		mine, err := encode(text, mask)
		if err != nil {
			t.Fatal(err)
		}
		if mine.Size != ref.Size {
			t.Errorf("%s: size %d, reference %d", name, mine.Size, ref.Size)
			continue
		}
		diff := 0
		for y := 0; y < ref.Size; y++ {
			for x := 0; x < ref.Size; x++ {
				if mine.Dark(x, y) != ref.Dark(x, y) {
					diff++
				}
			}
		}
		if diff != 0 {
			t.Errorf("%s (v%d, mask %d): %d of %d modules differ from the reference", name, mine.Version, mask, diff, ref.Size*ref.Size)
		}
		compared++
	}
	if compared == 0 {
		t.Skip("no .ref files in QR_PNG_DIR")
	}
	t.Logf("%d codes compared against qrencode", compared)
}

// readFormat decodes the 15 format bits beside the top-left finder: the
// BCH code must be a valid codeword naming level M and the chosen mask.
func readFormat(c *Code) (level, mask int, ok bool) {
	var bits int
	for i := 0; i < 15; i++ {
		var d bool
		switch {
		case i < 6:
			d = c.Dark(8, i)
		case i < 8:
			d = c.Dark(8, i+1)
		default:
			d = c.Dark(14-i+boolInt(i == 8), 8)
		}
		if d {
			bits |= 1 << uint(i)
		}
	}
	for m := 0; m < 8; m++ {
		if formatBits(m) == bits {
			return 0, m, true // the level M encoding is the only one we make
		}
	}
	return 0, 0, false
}

func TestEveryVersionHasItsStructure(t *testing.T) {
	outDir := os.Getenv("QR_PNG_DIR")
	for name, text := range samples() {
		c, err := Encode(text)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Size != 17+4*c.Version {
			t.Errorf("%s: version %d has size %d", name, c.Version, c.Size)
		}
		if strings.HasPrefix(name, "v") {
			var want int
			fmt.Sscanf(name, "v%d", &want)
			if c.Version != want {
				t.Errorf("%s: landed in version %d", name, c.Version)
			}
		}
		// Finder patterns: a dark 3x3 centre in each corner, light ring at distance 2.
		for _, p := range [][2]int{{3, 3}, {c.Size - 4, 3}, {3, c.Size - 4}} {
			if !c.Dark(p[0], p[1]) || c.Dark(p[0]+2, p[1]) || !c.Dark(p[0]+3, p[1]) {
				t.Errorf("%s: finder at %v malformed", name, p)
			}
		}
		// Timing patterns alternate between the finders.
		for i := 8; i < c.Size-8; i++ {
			if c.Dark(i, 6) != (i%2 == 0) || c.Dark(6, i) != (i%2 == 0) {
				t.Errorf("%s: timing pattern broken at %d", name, i)
				break
			}
		}
		if !c.Dark(8, c.Size-8) {
			t.Errorf("%s: the dark module is light", name)
		}
		if _, m, ok := readFormat(c); !ok || m != c.Mask {
			t.Errorf("%s: format information does not decode to mask %d (ok %v, got %d)", name, c.Mask, ok, m)
		}
		if c.Version >= 7 {
			// Version information, top-right copy.
			var bits int
			for i := 0; i < 18; i++ {
				if c.Dark(c.Size-11+i%3, i/3) {
					bits |= 1 << uint(i)
				}
			}
			if bits != versionBits(c.Version) {
				t.Errorf("%s: version information wrong (%018b)", name, bits)
			}
		}
		svg := c.SVG()
		if !strings.HasPrefix(svg, "<svg ") || strings.Contains(svg, "<script") || !strings.Contains(svg, fmt.Sprintf(`viewBox="0 0 %d %d"`, c.Size+8, c.Size+8)) {
			t.Errorf("%s: svg malformed", name)
		}
		if outDir != "" {
			if err := toPNG(c, filepath.Join(outDir, name+".png")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outDir, name+".txt"), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestTooLongIsAnError(t *testing.T) {
	if _, err := Encode(strings.Repeat("x", 214)); err != ErrTooLong {
		t.Fatalf("214 bytes: %v", err)
	}
	if _, err := Encode(strings.Repeat("x", 213)); err != nil {
		t.Fatalf("213 bytes: %v", err)
	}
}

// A Reed-Solomon codeword (data followed by its check bytes) is a multiple
// of the generator polynomial, so every generator root is a zero of it.
// Checked over GF(256) by Horner's rule, which shares nothing with the
// encoder's long division.
func TestReedSolomonCodewordsVanishAtEveryRoot(t *testing.T) {
	for _, n := range []int{10, 16, 18, 22, 24, 26} {
		data := []byte("the quick brown fox jumps over the lazy dog 0123456789")
		gen := generator(n)
		word := append(append([]byte(nil), data...), rsRemainder(data, gen)...)
		for root := 0; root < n; root++ {
			alpha := expT[root]
			var acc byte
			for _, c := range word {
				acc = gmul(acc, alpha) ^ c
			}
			if acc != 0 {
				t.Fatalf("%d check bytes: codeword is %02X at root alpha^%d, want 0", n, acc, root)
			}
		}
		// And a corrupted word is not: the check is not vacuous.
		word[3] ^= 0x40
		var acc byte
		for _, c := range word {
			acc = gmul(acc, expT[0]) ^ c
		}
		if acc == 0 {
			t.Fatalf("%d check bytes: a corrupted codeword still vanished", n)
		}
	}
}

// With QR_MASK_DIR set, the version-7 sample is written once per mask, for
// a decoder to read: a decoder that fails one mask and not the others points
// at the format information or the mask function for that mask.
func TestWriteEveryMask(t *testing.T) {
	dir := os.Getenv("QR_MASK_DIR")
	if dir == "" {
		t.Skip("QR_MASK_DIR not set")
	}
	text := samples()["v07"]
	for m := 0; m < 8; m++ {
		c, err := encode(text, m)
		if err != nil {
			t.Fatal(err)
		}
		if err := toPNG(c, filepath.Join(dir, fmt.Sprintf("v07-m%d.png", m))); err != nil {
			t.Fatal(err)
		}
	}
	auto, _ := Encode(text)
	t.Logf("the penalty rules pick mask %d for v07", auto.Mask)
}
