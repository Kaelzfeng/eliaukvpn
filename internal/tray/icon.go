//go:build windows

package tray

import (
	"bytes"
	"encoding/binary"
)

// makeIconICO renders a 32x32 tray icon in memory as a single-image ICO
// (green disc with a white "E", stylized like a network node) and returns the
// ICO file bytes. No binary assets are shipped; the icon is drawn at startup.
//
// ICO layout: ICONDIR (6 B) + one ICONDIRENTRY (16 B) + a 32-bit BGRA DIB
// (BITMAPINFOHEADER + pixel rows bottom-up) + a 1-bit AND mask.
func makeIconICO() []byte {
	const size = 32
	const cx, cy = 15.5, 15.5 // center
	const radius = 13.5

	// Draw BGRA, bottom-up (row 0 in the buffer is the bottom scanline).
	pix := make([]byte, size*size*4)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)-cx, float64(y)-cy
			in := dx*dx+dy*dy <= radius*radius
			var r, g, b, a byte
			if in {
				r, g, b, a = 0x1f, 0xb8, 0x53, 0xff // Eliauk green
				// Three horizontal white bars forming an "E".
				bar := (y >= 10 && y <= 12) || (y >= 14 && y <= 16) || (y >= 18 && y <= 20)
				if bar && x >= 9 && x <= 23 {
					if !(y >= 14 && y <= 16 && x >= 20) { // middle bar is short
						r, g, b = 0xff, 0xff, 0xff
					}
				}
			}
			row := size - 1 - y
			off := (row*size + x) * 4
			pix[off+0], pix[off+1], pix[off+2], pix[off+3] = b, g, r, a
		}
	}

	andMask := make([]byte, (size*size)/8) // all zeros -> fully opaque region

	var b bytes.Buffer
	// ICONDIR
	binary.Write(&b, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&b, binary.LittleEndian, uint16(1)) // type: icon
	binary.Write(&b, binary.LittleEndian, uint16(1)) // count
	// ICONDIRENTRY
	b.WriteByte(size)
	b.WriteByte(size)
	b.WriteByte(0)                                    // palette colors: 0 = 32-bit
	b.WriteByte(0)                                    // reserved
	binary.Write(&b, binary.LittleEndian, uint16(1))  // planes
	binary.Write(&b, binary.LittleEndian, uint16(32)) // bpp
	resSize := 40 + len(pix) + len(andMask)
	binary.Write(&b, binary.LittleEndian, uint32(resSize))
	binary.Write(&b, binary.LittleEndian, uint32(6+16)) // offset of image data
	// BITMAPINFOHEADER
	binary.Write(&b, binary.LittleEndian, uint32(40))
	binary.Write(&b, binary.LittleEndian, uint32(size))
	binary.Write(&b, binary.LittleEndian, uint32(size*2)) // height doubled (XOR + AND)
	binary.Write(&b, binary.LittleEndian, uint16(1))      // planes
	binary.Write(&b, binary.LittleEndian, uint16(32))     // bpp
	binary.Write(&b, binary.LittleEndian, uint32(0))      // BI_RGB
	binary.Write(&b, binary.LittleEndian, uint32(size*size*4))
	binary.Write(&b, binary.LittleEndian, uint32(0)) // x pixels/m
	binary.Write(&b, binary.LittleEndian, uint32(0)) // y pixels/m
	binary.Write(&b, binary.LittleEndian, uint32(0)) // colors used
	binary.Write(&b, binary.LittleEndian, uint32(0)) // important colors
	b.Write(pix)
	b.Write(andMask)
	return b.Bytes()
}
