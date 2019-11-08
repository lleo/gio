// SPDX-License-Identifier: Unlicense OR MIT

// +build linux,!android,!nox11 freebsd

package window

/*
#cgo LDFLAGS: -lX11
#include <stdlib.h>
#include <locale.h>
#include <X11/Xlib.h>
#include <X11/Xatom.h>
#include <X11/Xutil.h>
#include <X11/Xresource.h>

void gio_x11_init_ime(Display *dpy, Window win, XIM *xim, XIC *xic) {
	// adjust locale temporarily for XOpenIM
	char *lc = setlocale(LC_CTYPE, NULL);
	setlocale(LC_CTYPE, "");
	XSetLocaleModifiers("");

	*xim = XOpenIM(dpy, 0, 0, 0);
	if (!*xim) {
		// fallback to internal input method
		XSetLocaleModifiers("@im=none");
		*xim = XOpenIM(dpy, 0, 0, 0);
	}

	// revert locale to prevent any unexpected side effects
	setlocale(LC_CTYPE, lc);

	*xic = XCreateIC(*xim,
		XNInputStyle, XIMPreeditNothing | XIMStatusNothing,
		XNClientWindow, win,
		XNFocusWindow, win,
		NULL);

	XSetICFocus(*xic);
}
*/
import "C"
import (
	"errors"
	"fmt"
	"image"
	"strconv"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"gioui.org/f32"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/io/system"
	syscall "golang.org/x/sys/unix"
)

type x11Window struct {
	w  Callbacks
	x  *C.Display
	xw C.Window

	evDelWindow C.Atom
	stage       system.Stage
	cfg         config
	width       int
	height      int
	xim         C.XIM
	xic         C.XIC
	notify      struct {
		read, write int
	}
	dead bool

	mu         sync.Mutex
	animating  bool
	frameReady bool
}

func (w *x11Window) SetAnimating(anim bool) {
	w.mu.Lock()
	w.animating = anim
	w.mu.Unlock()
	if anim {
		w.wakeup()
	}
}

func (w *x11Window) ShowTextInput(show bool) {}

var x11OneByte = make([]byte, 1)

func (w *x11Window) wakeup() {
	if _, err := syscall.Write(w.notify.write, x11OneByte); err != nil && err != syscall.EAGAIN {
		panic(fmt.Errorf("failed to write to pipe: %v", err))
	}
}

func (w *x11Window) display() unsafe.Pointer {
	return unsafe.Pointer(w.x)
}

func (w *x11Window) setStage(s system.Stage) {
	if s == w.stage {
		return
	}
	w.stage = s
	w.w.Event(system.StageEvent{s})
}

func (w *x11Window) loop() {
	h := x11EventHandler{w: w, xev: new(C.XEvent), text: make([]byte, 4)}
	xfd := C.XConnectionNumber(w.x)

	// Poll for events and notifications.
	pollfds := []syscall.PollFd{
		{Fd: int32(xfd), Events: syscall.POLLIN | syscall.POLLERR},
		{Fd: int32(w.notify.read), Events: syscall.POLLIN | syscall.POLLERR},
	}
	xEvents := &pollfds[0].Revents
	// Plenty of room for a backlog of notifications.
	buf := make([]byte, 100)

loop:
	for !w.dead {
		time.Sleep(10*time.Millisecond)
		var syn, redraw bool
		// Check for pending draw events before checking animation or blocking.
		// This fixes an issue on Xephyr where on startup XPending() > 0 but
		// poll will still block. This also prevents no-op calls to poll.
		if syn = h.handleEvents(); !syn {
			w.mu.Lock()
			animating := w.animating
			redraw = animating && w.frameReady
			w.frameReady = false
			w.mu.Unlock()
			// Clear poll events.
			*xEvents = 0
			// Wait for X event or gio notification.
			if _, err := syscall.Poll(pollfds, -1); err != nil && err != syscall.EINTR {
				panic(fmt.Errorf("x11 loop: poll failed: %w", err))
			}
			switch {
			case *xEvents&syscall.POLLIN != 0:
				syn = h.handleEvents()
				if w.dead {
					break loop
				}
			case *xEvents&(syscall.POLLERR|syscall.POLLHUP) != 0:
				break loop
			}
		}
		// Clear notifications.
		for {
			_, err := syscall.Read(w.notify.read, buf)
			if err == syscall.EAGAIN {
				break
			}
			if err != nil {
				panic(fmt.Errorf("x11 loop: read from notify pipe failed: %w", err))
			}
			redraw = true
		}

		if redraw || syn {
			w.cfg.now = time.Now()
			w.w.Event(FrameEvent{
				FrameEvent: system.FrameEvent{
					Size: image.Point{
						X: w.width,
						Y: w.height,
					},
					Config: &w.cfg,
				},
				Sync: syn,
			})
		}
	}
	w.w.Event(system.DestroyEvent{Err: nil})
}

func (w *x11Window) eglFrameDone() {
	w.mu.Lock()
	anim := w.animating
	if anim {
		w.frameReady = true
	}
	w.mu.Unlock()
	if anim {
		w.wakeup()
	}
}

func (w *x11Window) destroy() {
	if w.notify.write != 0 {
		syscall.Close(w.notify.write)
		w.notify.write = 0
	}
	if w.notify.read != 0 {
		syscall.Close(w.notify.read)
		w.notify.read = 0
	}
	C.XDestroyIC(w.xic)
	C.XCloseIM(w.xim)
	C.XDestroyWindow(w.x, w.xw)
	C.XCloseDisplay(w.x)
}

// x11EventHandler wraps static variables for the main event loop.
// Its sole purpose is to prevent heap allocation and reduce clutter
// in x11window.loop.
//
type x11EventHandler struct {
	w      *x11Window
	text   []byte
	xev    *C.XEvent
	status C.Status
	keysym C.KeySym
}

// handleEvents returns true if the window needs to be redrawn.
//
func (h *x11EventHandler) handleEvents() bool {
	w := h.w
	xev := h.xev
	redraw := false
	for C.XPending(w.x) != 0 {
		C.XNextEvent(w.x, xev)
		if C.XFilterEvent(xev, C.None) == C.True {
			continue
		}
		switch (*C.XAnyEvent)(unsafe.Pointer(xev))._type {
		case C.KeyPress:
			kevt := (*C.XKeyPressedEvent)(unsafe.Pointer(xev))
		lookup:
			// Save state then clear CTRL & Shift bits in order to have
			// Xutf8LookupString return the unmodified key name in text[:l].
			//
			// Note that this enables sending a key.Event for key combinations
			// like CTRL-SHIFT-/ on QWERTY layouts, but CTRL-? is completely
			// masked. The same applies to AZERTY layouts where CTRL-SHIFT-É is
			// available but not CTRL-2.
			state := kevt.state
			mods := x11KeyStateToModifiers(state)
			if mods.Contain(key.ModCommand) {
				kevt.state &^= (C.uint(C.ControlMask) | C.uint(C.ShiftMask))
			}
			l := int(C.Xutf8LookupString(w.xic, kevt,
				(*C.char)(unsafe.Pointer(&h.text[0])), C.int(len(h.text)),
				&h.keysym, &h.status))
			switch h.status {
			case C.XBufferOverflow:
				h.text = make([]byte, l)
				goto lookup
			case C.XLookupChars:
				// Synthetic event from XIM.
				w.w.Event(key.EditEvent{Text: string(h.text[:l])})
			case C.XLookupKeySym:
				// Special keys.
				if r, ok := x11SpecialKeySymToRune(h.keysym); ok {
					w.w.Event(key.Event{
						Name:      r,
						Modifiers: mods,
					})
				}
			case C.XLookupBoth:
				if r, ok := x11SpecialKeySymToRune(h.keysym); ok {
					w.w.Event(key.Event{Name: r, Modifiers: mods})
				} else {
					if r, _ = utf8.DecodeRune(h.text[:l]); r != utf8.RuneError {
						w.w.Event(key.Event{Name: unicode.ToUpper(r), Modifiers: mods})
					}
					// Send EditEvent only when not a CTRL key combination.
					if !mods.Contain(key.ModCommand) {
						w.w.Event(key.EditEvent{Text: string(h.text[:l])})
					}
				}
			}
		case C.KeyRelease:
		case C.ButtonPress, C.ButtonRelease:
			bevt := (*C.XButtonEvent)(unsafe.Pointer(xev))
			ev := pointer.Event{
				Type:   pointer.Press,
				Source: pointer.Mouse,
				Position: f32.Point{
					X: float32(bevt.x),
					Y: float32(bevt.y),
				},
				Time: time.Duration(bevt.time) * time.Millisecond,
			}
			if bevt._type == C.ButtonRelease {
				ev.Type = pointer.Release
			}
			const scrollScale = 10
			switch bevt.button {
			case C.Button1:
				// left click by default
			case C.Button4:
				// scroll up
				ev.Type = pointer.Move
				ev.Scroll.Y = -scrollScale
			case C.Button5:
				// scroll down
				ev.Type = pointer.Move
				ev.Scroll.Y = +scrollScale
			default:
				continue
			}
			w.w.Event(ev)
		case C.MotionNotify:
			mevt := (*C.XMotionEvent)(unsafe.Pointer(xev))
			w.w.Event(pointer.Event{
				Type:   pointer.Move,
				Source: pointer.Mouse,
				Position: f32.Point{
					X: float32(mevt.x),
					Y: float32(mevt.y),
				},
				Time: time.Duration(mevt.time) * time.Millisecond,
			})
		case C.Expose: // update
			// redraw only on the last expose event
			redraw = (*C.XExposeEvent)(unsafe.Pointer(xev)).count == 0
		case C.FocusIn:
			w.w.Event(key.FocusEvent{Focus: true})
		case C.FocusOut:
			w.w.Event(key.FocusEvent{Focus: false})
		case C.ConfigureNotify: // window configuration change
			cevt := (*C.XConfigureEvent)(unsafe.Pointer(xev))
			w.width = int(cevt.width)
			w.height = int(cevt.height)
			// redraw will be done by a later expose event
		case C.ClientMessage: // extensions
			cevt := (*C.XClientMessageEvent)(unsafe.Pointer(xev))
			switch *(*C.long)(unsafe.Pointer(&cevt.data)) {
			case C.long(w.evDelWindow):
				w.dead = true
				return false
			}
		}
	}
	return redraw
}

func x11KeyStateToModifiers(s C.uint) key.Modifiers {
	var m key.Modifiers
	if s&C.ControlMask != 0 {
		m |= key.ModCommand
	}
	if s&C.ShiftMask != 0 {
		m |= key.ModShift
	}
	return m
}

func x11SpecialKeySymToRune(s C.KeySym) (rune, bool) {
	var n rune
	switch s {
	case C.XK_Escape:
		n = key.NameEscape
	case C.XK_Left, C.XK_KP_Left:
		n = key.NameLeftArrow
	case C.XK_Right, C.XK_KP_Right:
		n = key.NameRightArrow
	case C.XK_Return:
		n = key.NameReturn
	case C.XK_KP_Enter:
		n = key.NameEnter
	case C.XK_Up, C.XK_KP_Up:
		n = key.NameUpArrow
	case C.XK_Down, C.XK_KP_Down:
		n = key.NameDownArrow
	case C.XK_Home, C.XK_KP_Home:
		n = key.NameHome
	case C.XK_End, C.XK_KP_End:
		n = key.NameEnd
	case C.XK_BackSpace:
		n = key.NameDeleteBackward
	case C.XK_Delete, C.XK_KP_Delete:
		n = key.NameDeleteForward
	case C.XK_Page_Up, C.XK_KP_Prior:
		n = key.NamePageUp
	case C.XK_Page_Down, C.XK_KP_Next:
		n = key.NamePageDown
	default:
		return 0, false
	}
	return n, true
}

var (
	x11Threads sync.Once
)

func init() {
	x11Driver = newX11Window
}

func newX11Window(gioWin Callbacks, opts *Options) error {
	var err error

	pipe := make([]int, 2)
	if err := syscall.Pipe2(pipe, syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
		return fmt.Errorf("NewX11Window: failed to create pipe: %w", err)
	}

	x11Threads.Do(func() {
		if C.XInitThreads() == 0 {
			err = errors.New("x11: threads init failed")
		}
		C.XrmInitialize()
	})
	if err != nil {
		return err
	}
	dpy := C.XOpenDisplay(nil)
	if dpy == nil {
		return errors.New("x11: cannot connect to the X server")
	}

	root := C.XDefaultRootWindow(dpy)
	screen := C.XDefaultScreen(dpy)
	ppsp := x11DetectUIScale(dpy, screen)
	cfg := config{pxPerDp: ppsp, pxPerSp: ppsp}
	var (
		swa C.XSetWindowAttributes
		xim C.XIM
		xic C.XIC
	)
	swa.event_mask = C.ExposureMask | C.PointerMotionMask | C.KeyPressMask
	swa.background_pixmap = C.None
	win := C.XCreateWindow(dpy, root,
		0, 0, C.uint(cfg.Px(opts.Width)), C.uint(cfg.Px(opts.Height)), 0,
		C.CopyFromParent, C.InputOutput,
		nil, C.CWEventMask|C.CWBackPixmap,
		&swa)
	C.gio_x11_init_ime(dpy, win, &xim, &xic)
	C.XSelectInput(dpy, win, 0|
		C.ExposureMask|C.FocusChangeMask| // update
		C.KeyPressMask|C.KeyReleaseMask| // keyboard
		C.ButtonPressMask|C.ButtonReleaseMask| // mouse clicks
		C.PointerMotionMask| // mouse movement
		C.StructureNotifyMask, // resize
	)

	w := &x11Window{
		w: gioWin, x: dpy, xw: win,
		width:  cfg.Px(opts.Width),
		height: cfg.Px(opts.Height),
		cfg:    cfg,
		xim:    xim,
		xic:    xic,
	}
	w.notify.read = pipe[0]
	w.notify.write = pipe[1]

	var xattr C.XSetWindowAttributes
	xattr.override_redirect = C.False
	C.XChangeWindowAttributes(dpy, win, C.CWOverrideRedirect, &xattr)

	var hints C.XWMHints
	hints.input = C.True
	hints.flags = C.InputHint
	C.XSetWMHints(dpy, win, &hints)

	// make the window visible on the screen
	C.XMapWindow(dpy, win)

	// set the name
	ctitle := C.CString(opts.Title)
	C.XStoreName(dpy, win, ctitle)
	C.free(unsafe.Pointer(ctitle))

	// extensions
	ckey := C.CString("WM_DELETE_WINDOW")
	w.evDelWindow = C.XInternAtom(dpy, ckey, C.False)
	C.free(unsafe.Pointer(ckey))
	C.XSetWMProtocols(dpy, win, &w.evDelWindow, 1)

	go func() {
		w.w.SetDriver(w)
		w.setStage(system.StageRunning)
		w.loop()
		w.destroy()
		close(mainDone)
	}()
	return nil
}

// detectUIScale reports the system UI scale, or 1.0 if it fails.
func x11DetectUIScale(dpy *C.Display, screen C.int) float32 {
	// default fixed DPI value used in most desktop UI toolkits
	const defaultDesktopDPI = 96
	var scale float32 = 1.0

	// Get actual DPI from X resource Xft.dpi (set by GTK and Qt).
	// This value is entirely based on user preferences and conflates both
	// screen (UI) scaling and font scale.
	rms := C.XResourceManagerString(dpy)
	if rms != nil {
		db := C.XrmGetStringDatabase(rms)
		if db != nil {
			var (
				t *C.char
				v C.XrmValue
			)
			if C.XrmGetResource(db, (*C.char)(unsafe.Pointer(&[]byte("Xft.dpi\x00")[0])),
				(*C.char)(unsafe.Pointer(&[]byte("Xft.Dpi\x00")[0])), &t, &v) != C.False {
				if t != nil && C.GoString(t) == "String" {
					f, err := strconv.ParseFloat(C.GoString(v.addr), 32)
					if err == nil {
						scale = float32(f) / defaultDesktopDPI
					}
				}
			}
			C.XrmDestroyDatabase(db)
		}
	}

	return scale
}
