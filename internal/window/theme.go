//go:build windows

// Dark theme for the Eliauk main window: dark cards with an indigo accent,
// owner-draw buttons with hover/press feedback, a status dot, and a dark title
// bar. Everything is plain GDI (RoundRect/Ellipse/FillRect/DrawTextW) — no
// third-party drawing library.
//
// Layout contract: the header strip (0..winH-headerHeight) hosts the app name,
// the status dot and the status line; below it the six section cards are
// painted by the parent window's WM_PAINT while the controls inside them are
// real child windows. Buttons are BS_OWNERDRAW and subclassed to track
// hover/press; native controls (edits, listbox) get their dark colors via
// WM_CTLCOLOR* and their dark internal theme via SetWindowTheme.
package window

import (
	"syscall"
	"unsafe"
)

// ---- owner-draw / theming constants ----
const (
	bsOwnerDraw = 0x0000000B // BS_OWNERDRAW
	ssOwnerDraw = 0x0000000D // SS_OWNERDRAW
	odsDisabled = 0x0020     // ODS_DISABLED
	odtButton   = 0x0004     // DRAWITEMSTRUCT ctlType: ODT_BUTTON (4, not 2)
	odtStatic   = 0x0005     // DRAWITEMSTRUCT ctlType: ODT_STATIC (5, not 1)

	// messages handled by the theme
	wmEraseBkgnd      = 0x0014
	wmPaint           = 0x000F
	wmDrawItem        = 0x002B
	wmCtlColorEdit    = 0x0133
	wmCtlColorListbox = 0x0134
	wmMouseMove       = 0x0200
	wmLButtonDown     = 0x0201
	wmLButtonUp       = 0x0202
	wmMouseLeave      = 0x02A3

	// DrawTextW format flags
	dtCenter     = 0x00000001
	dtVCenter    = 0x00000004
	dtSingleLine = 0x00000020
	dtNoPrefix   = 0x00000800

	psSolid = 0 // PS_SOLID
	tmeLeave = 0x0002 // TME_LEAVE

	dwmwaUseImmersiveDarkMode = 20 // DWMWA_USE_IMMERSIVE_DARK_MODE
)

// gwlpWndProc is GWLP_WNDPROC (-4) as an unsigned bit pattern.
var gwlpWndProc = ^uintptr(3)

// ---- palette (COLORREF = 0x00BBGGRR) ----
const (
	colBg        = 0x001E1F22 // window background (very dark gray-blue)
	colCard      = 0x00242529 // card surface
	colCardEdge  = 0x00302F34 // card border
	colInput     = 0x002B2D32 // edit/listbox background
	colText      = 0x00EDEFF3 // primary text (near white)
	colMuted     = 0x008B8E95 // secondary text
	colWhite     = 0x00FFFFFF

	// indigo accent (#6C5CE7) with hover/pressed variants
	colAccent    = 0x00E75C6C
	colAccentHov = 0x00EF6B7C
	colAccentPrs = 0x00D44B5A

	// secondary button
	colBtn2     = 0x00332F2E
	colBtn2Hov  = 0x00403B3A
	colBtn2Prs  = 0x00292524
	colBtn2Edge = 0x00423D3E

	// disabled button
	colBtnDis    = 0x00292423
	colBtnDisTxt = 0x005A5D63

	// status dot / status line
	colGreen = 0x006FDC3D // #3DDC6F
	colRed   = 0x005C5CFF // #FF5C5C
)

// ---- section cards ----
var (
	cardRects  = [6]rect{
		{8, 44, 512, 136},   // 账号 (title 58..76, controls 76..134)
		{8, 142, 512, 258},  // 好友 (title 156..174, controls 172..256)
		{8, 264, 512, 352},  // 房间 (title 278..296, controls 296..348)
		{8, 358, 512, 506},  // 游戏 (title 372..390, controls 388..500)
		{8, 512, 512, 668},  // 连接状态 (title 526..544, controls 546..666)
		{8, 674, 512, 790},  // 设置 (title 688..706, controls 706..782)
	}
	cardTitles = [6]string{"账号", "好友", "房间", "游戏", "连接状态", "设置"}
)

// primaryBtn lists the buttons drawn with the indigo accent fill; everything
// else is a flat secondary button.
var primaryBtn = map[uintptr]bool{
	idcLogin:      true,
	idcRegister:   true,
	idcSave:       true,
	idcRoomCreate: true,
	idcRoomJoin:   true,
	idcAdd:        true,
	idcGameStart:  true,
}

// btnState is the per-button hover/press state, populated by the subclassed
// window proc.
type btnState struct {
	hovered bool
	pressed bool
	oldProc uintptr
}

// btnStates maps a button HWND to its state.
var btnStates = map[uintptr]*btnState{}

// makeFont creates a Segoe UI font of the given pixel height and weight.
func makeFont(height, weight int32) uintptr {
	face, _ := syscall.UTF16PtrFromString("Segoe UI")
	h, _, _ := procCreateFontW.Call(
		uintptr(-height), 0, 0, 0, uintptr(weight), 0, 0, 0, 1, // DEFAULT_CHARSET
		0, 0, 5, 0, uintptr(unsafe.Pointer(face))) // CLEARTYPE_QUALITY
	return h
}

// loadThemeBrushes creates the persistent solid brushes used for control
// backgrounds. They live for the life of the window.
func (w *Window) loadThemeBrushes() {
	w.brBg, _, _ = procCreateSolidBrush.Call(uintptr(colBg))
	w.brCard, _, _ = procCreateSolidBrush.Call(uintptr(colCard))
	w.brInput, _, _ = procCreateSolidBrush.Call(uintptr(colInput))
}

// applyImmersiveDark turns on the dark caption bar (Windows 10 1809+).
func applyImmersiveDark(hwnd uintptr) {
	one := uint32(1)
	procDwmSetWindowAttribute.Call(hwnd, dwmwaUseImmersiveDarkMode, uintptr(unsafe.Pointer(&one)), 4)
}

// applyDarkThemeControl switches a native control to the dark internal theme
// (dark borders for edits, dark selection colors for the listbox).
func applyDarkThemeControl(h uintptr) {
	s, _ := syscall.UTF16PtrFromString("DarkMode_Explorer")
	procSetWindowTheme.Call(h, uintptr(unsafe.Pointer(s)), 0)
}

// themeMessage handles the drawing/theming messages the theme owns. It returns
// (result, handled).
func (w *Window) themeMessage(m uint32, wParam, lParam uintptr) (uintptr, bool) {
	switch m {
	case wmEraseBkgnd:
		// Fill the whole client with the background color once; the cards are
		// painted on top by WM_PAINT.
		rc := rect{0, 0, winW, winH}
		br, _, _ := procCreateSolidBrush.Call(uintptr(colBg))
		procFillRect.Call(wParam, uintptr(unsafe.Pointer(&rc)), br)
		procDeleteObject.Call(br)
		return 1, true

	case wmPaint:
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(w.hwnd, uintptr(unsafe.Pointer(&ps)))
		if hdc != 0 {
			w.paintCards(hdc)
			procEndPaint.Call(w.hwnd, uintptr(unsafe.Pointer(&ps)))
		}
		return 0, true

	case wmDrawItem:
		if lParam != 0 {
			// lParam carries a DRAWITEMSTRUCT pointer; launder the bits through
			// memory to satisfy vet's unsafeptr analyzer (same idiom as the
			// clipboard/elevation code).
			ptr := *(*unsafe.Pointer)(unsafe.Pointer(&lParam))
			ds := (*drawItemStruct)(ptr)
			switch ds.ctlType {
			case odtButton:
				w.drawButton(ds)
			case odtStatic:
				w.drawStatusDot(ds)
			}
			return 1, true
		}

	case wmCtlColorStatic:
		return w.ctlColor(wParam, lParam), true

	case wmCtlColorEdit, wmCtlColorListbox:
		procSetTextColor.Call(lParam, uintptr(colText))
		procSetBkColor.Call(lParam, uintptr(colInput))
		return w.brInput, true
	}
	return 0, false
}

// ctlColor paints a STATIC's text color over its card/background brush.
func (w *Window) ctlColor(hwnd, hdc uintptr) uintptr {
	var col uint32 = colText
	switch {
	case hwnd == w.statusTxt:
		col = w.dotColor()
	default:
		if c, ok := w.staticColors[hwnd]; ok {
			col = c
		}
	}
	procSetTextColor.Call(hdc, uintptr(col))
	procSetBkMode.Call(hdc, 1) // TRANSPARENT
	if hwnd == w.statusTxt || hwnd == w.titleTxt {
		return w.brBg
	}
	return w.brCard
}

// paintCards draws the six rounded section cards with their accent bar and
// section title. Called from WM_PAINT.
func (w *Window) paintCards(hdc uintptr) {
	for i := range cardRects {
		r := cardRects[i]
		drawRoundedRect(hdc, r, colCard, colCardEdge)

		// accent bar
		bar := rect{r.left + 13, r.top + 15, r.left + 17, r.top + 31}
		br, _, _ := procCreateSolidBrush.Call(uintptr(colAccent))
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&bar)), br)
		procDeleteObject.Call(br)

		// section title
		procSetTextColor.Call(hdc, uintptr(colText))
		procSetBkMode.Call(hdc, 1)
		procSelectObject.Call(hdc, w.fSection)
		t, _ := syscall.UTF16PtrFromString(cardTitles[i])
		rc := rect{r.left + 26, r.top + 14, r.right - 12, r.top + 32}
		procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(t)), ^uintptr(0), // -1: null-terminated
			uintptr(unsafe.Pointer(&rc)),
			uintptr(dtVCenter|dtSingleLine|dtNoPrefix))
	}
}

// drawRoundedRect fills and outlines r with a rounded (radius ~7px) rectangle.
// A zero fill or edge color omits that part.
func drawRoundedRect(hdc uintptr, r rect, fill, edge uint32) {
	if fill == 0 && edge == 0 {
		return
	}
	var br, pen uintptr
	if fill != 0 {
		br, _, _ = procCreateSolidBrush.Call(uintptr(fill))
		procSelectObject.Call(hdc, br)
	}
	if edge != 0 {
		pen, _, _ = procCreatePen.Call(psSolid, 1, uintptr(edge))
		procSelectObject.Call(hdc, pen)
	}
	procRoundRect.Call(hdc, uintptr(r.left), uintptr(r.top), uintptr(r.right), uintptr(r.bottom), 14, 14)
	if pen != 0 {
		procDeleteObject.Call(pen)
	}
	if br != 0 {
		procDeleteObject.Call(br)
	}
}

// drawDot paints a filled circle of the given color.
func drawDot(hdc uintptr, r rect, color uint32) {
	br, _, _ := procCreateSolidBrush.Call(uintptr(color))
	pen, _, _ := procCreatePen.Call(psSolid, 1, uintptr(color))
	procSelectObject.Call(hdc, br)
	procSelectObject.Call(hdc, pen)
	procEllipse.Call(hdc, uintptr(r.left), uintptr(r.top), uintptr(r.right), uintptr(r.bottom))
	procDeleteObject.Call(pen)
	procDeleteObject.Call(br)
}

// dotColor resolves the current status to the dot/text color.
func (w *Window) dotColor() uint32 {
	switch w.statusDot {
	case 1:
		return colGreen
	case 2:
		return colRed
	}
	return colMuted
}

// setStatusDot updates the status indicator and repaints the dot + status line.
// dot: 0=neutral/connecting, 1=good, 2=bad. UI thread only.
func (w *Window) setStatusDot(dot int8) {
	if dot < 0 || dot > 2 {
		dot = 0
	}
	if w.statusDot == dot {
		return
	}
	w.statusDot = dot
	if w.statusDotHwnd != 0 {
		procInvalidateRect.Call(w.statusDotHwnd, 0, 1)
	}
	if w.statusTxt != 0 {
		procInvalidateRect.Call(w.statusTxt, 0, 1)
	}
}

// drawStatusDot paints the owner-draw status dot: the window background behind
// it, then the dot itself.
func (w *Window) drawStatusDot(ds *drawItemStruct) {
	br, _, _ := procCreateSolidBrush.Call(uintptr(colBg))
	procFillRect.Call(ds.hDC, uintptr(unsafe.Pointer(&ds.rcItem)), br)
	procDeleteObject.Call(br)
	r := ds.rcItem
	r.left += 1
	r.top += 1
	r.right -= 1
	r.bottom -= 1
	drawDot(ds.hDC, r, w.dotColor())
}

// drawButton paints an owner-draw button with the primary/secondary look and
// hover/press feedback.
func (w *Window) drawButton(ds *drawItemStruct) {
	st := btnStates[ds.hwndItem]
	primary := primaryBtn[uintptr(ds.ctlID)]
	disabled := ds.itemState&odsDisabled != 0
	hovered := st != nil && st.hovered && !disabled
	pressed := st != nil && st.pressed && !disabled

	var fill, edge, txt uint32
	switch {
	case disabled:
		fill, edge, txt = colBtnDis, colBtnDis, colBtnDisTxt
	case primary:
		fill, txt = colAccent, colWhite
		if pressed {
			fill = colAccentPrs
		} else if hovered {
			fill = colAccentHov
		}
		edge = fill
	default:
		fill, txt = colBtn2, colText
		if pressed {
			fill = colBtn2Prs
		} else if hovered {
			fill = colBtn2Hov
		}
		edge = colBtn2Edge
	}

	drawRoundedRect(ds.hDC, ds.rcItem, fill, edge)

	var buf [128]uint16
	n, _, _ := procGetWindowTextW.Call(ds.hwndItem, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n > 0 {
		procSetTextColor.Call(ds.hDC, uintptr(txt))
		procSetBkMode.Call(ds.hDC, 1)
		procSelectObject.Call(ds.hDC, w.fBody)
		rc := ds.rcItem
		rc.left += 2
		rc.right -= 2
		procDrawTextW.Call(ds.hDC, uintptr(unsafe.Pointer(&buf[0])), uintptr(int32(n)),
			uintptr(unsafe.Pointer(&rc)),
			uintptr(dtCenter|dtVCenter|dtSingleLine|dtNoPrefix))
	}
}

// subclassButton replaces a button's window proc to track hover/press so the
// owner-draw renderer can react. The original proc is chained via
// CallWindowProcW.
func (w *Window) subclassButton(h uintptr) {
	old, _, _ := procGetWindowLongPtrW.Call(h, gwlpWndProc)
	st := &btnState{oldProc: old}
	btnStates[h] = st

	cb := syscall.NewCallback(func(hwnd uintptr, m uint32, wParam, lParam uintptr) uintptr {
		s := btnStates[hwnd]
		if s != nil {
			switch m {
			case wmMouseMove:
				if !s.hovered {
					s.hovered = true
					procInvalidateRect.Call(hwnd, 0, 1)
					tme := &trackMouseEvent{cbSize: 20, dwFlags: tmeLeave, hwndTrack: hwnd}
					procTrackMouseEvent.Call(uintptr(unsafe.Pointer(tme)))
				}
			case wmMouseLeave:
				s.hovered = false
				if !s.pressed {
					procInvalidateRect.Call(hwnd, 0, 1)
				}
			case wmLButtonDown:
				s.pressed = true
				procSetCapture.Call(hwnd)
				procInvalidateRect.Call(hwnd, 0, 1)
			case wmLButtonUp:
				s.pressed = false
				procReleaseCapture.Call()
				procInvalidateRect.Call(hwnd, 0, 1)
			}
			r, _, _ := procCallWindowProcW.Call(s.oldProc, hwnd, uintptr(m), wParam, lParam)
			return r
		}
		return 0
	})
	w.btnCallbacks = append(w.btnCallbacks, cb)
	procSetWindowLongPtrW.Call(h, gwlpWndProc, cb)
}
