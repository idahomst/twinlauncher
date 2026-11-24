package main

// gui.go — Win32 GUI built with lxn/walk

import (
	"context"
	"fmt"
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

// cancelUpdate is non-nil while an update runs. Only touched on the UI thread.
var cancelUpdate context.CancelFunc

// Walk widget references (set during window creation)
var (
	mw        *walk.MainWindow
	rbCata    *walk.RadioButton
	rbMop     *walk.RadioButton
	rbVanilla *walk.RadioButton
	lePath    *walk.LineEdit
	btnBrowse *walk.PushButton
	leRL      *walk.LineEdit
	btnRLPick *walk.PushButton
	langRow   *walk.Composite
	leLang    *walk.LineEdit
	btnLang   *walk.PushButton
	chkClr    *walk.CheckBox
	chkSetRL  *walk.CheckBox
	btnUpdate *walk.PushButton
	btnPlay   *walk.PushButton
	pb        *walk.ProgressBar
	lblStatus *walk.Label
	teLog     *walk.TextEdit
)

const labelWidth = 80

// ── UI update helpers (safe to call from any goroutine) ──────────────────────

func logLine(s string) {
	if mw == nil {
		return
	}
	mw.Synchronize(func() { appendLog(s) })
}

func setStatus(s string) {
	if mw == nil {
		return
	}
	mw.Synchronize(func() { lblStatus.SetText(s) })
}

func setPercent(p int) {
	if mw == nil {
		return
	}
	mw.Synchronize(func() { pb.SetValue(p) })
}

// msgBox works before the main window exists (walk.MsgBox would dereference
// a nil *MainWindow wrapped in a non-nil walk.Form).
func msgBox(title, text string, style walk.MsgBoxStyle) {
	var owner walk.Form
	if mw != nil {
		owner = mw
	}
	walk.MsgBox(owner, title, text, style)
}

const emGetTextLength = 0x000E // WM_GETTEXTLENGTH (not exposed by lxn/win)

// appendLog appends a line to the log TextEdit efficiently using Win32
// EM_SETSEL / EM_REPLACESEL so we never have to read back the whole text.
func appendLog(line string) {
	hwnd := teLog.Handle()
	l := win.SendMessage(hwnd, emGetTextLength, 0, 0)
	win.SendMessage(hwnd, win.EM_SETSEL, l, l)
	p, _ := syscall.UTF16PtrFromString(line + "\r\n")
	win.SendMessage(hwnd, win.EM_REPLACESEL, 0, uintptr(unsafe.Pointer(p)))
}

// ── Native dialogs / menus (direct syscalls work reliably under Wine) ────────

var (
	shell32dll           = syscall.NewLazyDLL("shell32.dll")
	ole32dll             = syscall.NewLazyDLL("ole32.dll")
	user32dll            = syscall.NewLazyDLL("user32.dll")
	_SHBrowseForFolder   = shell32dll.NewProc("SHBrowseForFolderW")
	_SHGetPathFromIDList = shell32dll.NewProc("SHGetPathFromIDListW")
	_CoTaskMemFree       = ole32dll.NewProc("CoTaskMemFree")
	_CreatePopupMenu     = user32dll.NewProc("CreatePopupMenu")
	_AppendMenuW         = user32dll.NewProc("AppendMenuW")
	_DestroyMenu         = user32dll.NewProc("DestroyMenu")
	_TrackPopupMenu      = user32dll.NewProc("TrackPopupMenu")
	_GetWindowRect       = user32dll.NewProc("GetWindowRect")
)

type browseInfo struct {
	HwndOwner      uintptr
	PidlRoot       uintptr
	PszDisplayName *uint16
	LpszTitle      *uint16
	UlFlags        uint32
	Lpfn           uintptr
	LParam         uintptr
	IImage         int32
}

const (
	bifReturnOnlyFSDirs = 0x0001
	bifNewDialogStyle   = 0x0040

	tpmLeftButton = 0x0000
	tpmRightAlign = 0x0008
	tpmReturnCmd  = 0x0100

	mfString  = 0x00000000
	mfChecked = 0x00000008
)

func browseFolder(parent uintptr, title string) string {
	dispBuf := make([]uint16, syscall.MAX_PATH)
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	bi := browseInfo{
		HwndOwner:      parent,
		PszDisplayName: &dispBuf[0],
		LpszTitle:      titlePtr,
		UlFlags:        bifReturnOnlyFSDirs | bifNewDialogStyle,
	}
	pidl, _, _ := _SHBrowseForFolder.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return ""
	}
	defer _CoTaskMemFree.Call(pidl)
	pathBuf := make([]uint16, syscall.MAX_PATH)
	_SHGetPathFromIDList.Call(pidl, uintptr(unsafe.Pointer(&pathBuf[0])))
	return syscall.UTF16ToString(pathBuf)
}

// popupMenu shows a native popup menu below anchor and returns the chosen
// index, or -1. The menu is right-aligned to the button so it stays inside
// the window. TrackPopupMenu is used because ComboBox drop-downs do not
// react to the mouse under Wine.
func popupMenu(anchor *walk.PushButton, items []string, checked int) int {
	hMenu, _, _ := _CreatePopupMenu.Call()
	if hMenu == 0 {
		return -1
	}
	defer _DestroyMenu.Call(hMenu)

	for i, it := range items {
		p, _ := syscall.UTF16PtrFromString(it)
		flags := uintptr(mfString)
		if i == checked {
			flags |= mfChecked
		}
		_AppendMenuW.Call(hMenu, flags, uintptr(i+1), uintptr(unsafe.Pointer(p)))
	}

	type rect32 struct{ Left, Top, Right, Bottom int32 }
	var r rect32
	_GetWindowRect.Call(uintptr(anchor.Handle()), uintptr(unsafe.Pointer(&r)))

	cmd, _, _ := _TrackPopupMenu.Call(hMenu, tpmReturnCmd|tpmLeftButton|tpmRightAlign,
		uintptr(r.Right), uintptr(r.Bottom), 0, uintptr(mw.Handle()), 0)
	if cmd == 0 || int(cmd) > len(items) {
		return -1
	}
	return int(cmd) - 1
}

// ── State helpers ────────────────────────────────────────────────────────────

func currentExpansion() string {
	switch {
	case rbCata != nil && rbCata.Checked():
		return "Cata"
	case rbMop != nil && rbMop.Checked():
		return "Mop"
	case rbVanilla != nil && rbVanilla.Checked():
		return "Vanilla"
	}
	return userCfg.Expansion
}

// readControls copies the control values into userCfg and saves them.
func readControls() {
	userCfg.Expansion = currentExpansion()
	setGamePath(lePath.Text())
	userCfg.Realmlist = leRL.Text()
	userCfg.ClearCache = chkClr.Checked()
	userCfg.SkipRealmlistSetup = !chkSetRL.Checked()
	saveUserSettings()
}

func localeLabel(code string) string {
	for _, l := range mopLocales {
		if l.Code == code {
			return l.Code + " - " + l.Name
		}
	}
	return code
}

// refreshExpansionUI enables the MoP-only language row. It is disabled rather
// than hidden: hiding a row at runtime leaves the layout unpainted under Wine.
func refreshExpansionUI() {
	langRow.SetEnabled(userCfg.Expansion == "Mop" && cancelUpdate == nil)
}

// setBusy enables/disables everything that must not change while an update
// is running.
func setBusy(busy bool) {
	for _, w := range []walk.Widget{rbCata, rbMop, rbVanilla, lePath, btnBrowse, btnPlay} {
		w.SetEnabled(!busy)
	}
	refreshExpansionUI()
	if busy {
		btnUpdate.SetText("Cancel")
	} else {
		btnUpdate.SetText("Check && Update")
	}
	btnUpdate.SetEnabled(true)
}

// ── Handlers ─────────────────────────────────────────────────────────────────

func onExpansionRadio(exp string) {
	userCfg.Expansion = exp
	saveUserSettings()
	lePath.SetText(gamePath())
	refreshExpansionUI()
}

func onBrowse() {
	p := browseFolder(uintptr(mw.Handle()), "Select WoW installation folder")
	if p == "" {
		return
	}
	lePath.SetText(p)
	setGamePath(p)
	saveUserSettings()
}

func onPickRealmlist() {
	rls := settings().Realmlists
	cur := -1
	for i, rl := range rls {
		if rl == leRL.Text() {
			cur = i
		}
	}
	if i := popupMenu(btnRLPick, rls, cur); i >= 0 {
		leRL.SetText(rls[i])
		userCfg.Realmlist = rls[i]
		saveUserSettings()
	}
}

func onPickLanguage() {
	items := make([]string, len(mopLocales))
	cur := -1
	for i, l := range mopLocales {
		items[i] = localeLabel(l.Code)
		if l.Code == userCfg.GameLocale {
			cur = i
		}
	}
	if i := popupMenu(btnLang, items, cur); i >= 0 {
		userCfg.GameLocale = mopLocales[i].Code
		leLang.SetText(items[i])
		saveUserSettings()
	}
}

func onUpdate() {
	if cancelUpdate != nil {
		cancelUpdate()
		btnUpdate.SetText("Cancelling…")
		btnUpdate.SetEnabled(false)
		return
	}

	readControls()
	exp, gp, mopLocale := userCfg.Expansion, gamePath(), userCfg.GameLocale
	if gp == "" {
		msgBox("Error", "Please set the game path first.", walk.MsgBoxIconError|walk.MsgBoxOK)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancelUpdate = cancel
	setBusy(true)
	pb.SetValue(0)
	lblStatus.SetText("Starting…")
	appendLog(fmt.Sprintf("=== Update %s in %s ===", exp, gp))

	go func() {
		refreshSettings(ctx)
		err := runUpdate(ctx, exp, gp, mopLocale)
		cancelled := ctx.Err() != nil
		mw.Synchronize(func() {
			cancelUpdate = nil
			cancel()
			setBusy(false)
			switch {
			case cancelled:
				lblStatus.SetText("Cancelled.")
				appendLog("=== Update cancelled ===")
			case err != nil:
				lblStatus.SetText("Error: " + err.Error())
				appendLog("ERROR: " + err.Error())
				msgBox("Update Error", err.Error(), walk.MsgBoxIconError|walk.MsgBoxOK)
			default:
				lblStatus.SetText("Done!")
				appendLog("=== Update complete! ===")
				pb.SetValue(100)
			}
		})
	}()
}

func onPlay() {
	readControls()
	doPlay()
}

func onClose(_ *bool, _ walk.CloseReason) {
	if cancelUpdate != nil {
		cancelUpdate()
	}
	readControls()
}

// ── Window ───────────────────────────────────────────────────────────────────

// row lays out "Label: [widgets…]" with the label column aligned. An empty
// label becomes a fixed spacer (an empty Label ignores its MinSize).
func row(label string, children ...Widget) Composite {
	var first Widget = Label{Text: label, MinSize: Size{Width: labelWidth}}
	if label == "" {
		first = HSpacer{Size: labelWidth}
	}
	return Composite{
		Layout:   HBox{MarginsZero: true, Spacing: 6},
		Children: append([]Widget{first}, children...),
	}
}

func pickButton(assign **walk.PushButton, onClick walk.EventHandler) PushButton {
	return PushButton{AssignTo: assign, Text: "…", MaxSize: Size{Width: 32}, OnClicked: onClick}
}

func runGUI() {
	if err := (MainWindow{
		AssignTo: &mw,
		Title:    "twinlauncher",
		MinSize:  Size{Width: 460, Height: 420},
		Size:     Size{Width: 460, Height: 440},
		Layout:   VBox{Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}, Spacing: 6},
		Children: []Widget{
			row("Expansion:",
				RadioButton{AssignTo: &rbCata, Text: "Cataclysm", OnClicked: func() { onExpansionRadio("Cata") }},
				RadioButton{AssignTo: &rbMop, Text: "Mists of Pandaria", OnClicked: func() { onExpansionRadio("Mop") }},
				RadioButton{AssignTo: &rbVanilla, Text: "Vanilla", OnClicked: func() { onExpansionRadio("Vanilla") }},
				HSpacer{},
			),
			row("Game path:",
				LineEdit{
					AssignTo:          &lePath,
					Text:              gamePath(),
					OnTextChanged:     func() { setGamePath(lePath.Text()) },
					OnEditingFinished: saveUserSettings,
				},
				pickButton(&btnBrowse, onBrowse),
			),
			row("Realmlist:",
				LineEdit{
					AssignTo:          &leRL,
					Text:              userCfg.Realmlist,
					OnEditingFinished: func() { userCfg.Realmlist = leRL.Text(); saveUserSettings() },
				},
				pickButton(&btnRLPick, onPickRealmlist),
			),
			Composite{
				AssignTo: &langRow,
				Layout:   HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					Label{Text: "Language:", MinSize: Size{Width: labelWidth}},
					LineEdit{AssignTo: &leLang, ReadOnly: true, Text: localeLabel(userCfg.GameLocale)},
					pickButton(&btnLang, onPickLanguage),
				},
			},
			row("",
				CheckBox{AssignTo: &chkClr, Text: "Clear cache on launch", Checked: userCfg.ClearCache},
				HSpacer{},
			),
			row("",
				CheckBox{
					AssignTo: &chkSetRL,
					Text:     "Set realmlist before launch",
					Checked:  !userCfg.SkipRealmlistSetup, // stored inverted
				},
				HSpacer{},
			),
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					PushButton{AssignTo: &btnUpdate, Text: "Check && Update", OnClicked: onUpdate},
					PushButton{AssignTo: &btnPlay, Text: "Play", OnClicked: onPlay},
				},
			},
			ProgressBar{AssignTo: &pb, MinValue: 0, MaxValue: 100},
			Label{AssignTo: &lblStatus, Text: "Ready."},
			TextEdit{AssignTo: &teLog, ReadOnly: true, VScroll: true, MinSize: Size{Height: 100}},
		},
	}).Create(); err != nil {
		panic(err)
	}

	// Lift the default 32K-character limit of the log edit control.
	teLog.SetMaxLength(0)

	switch userCfg.Expansion {
	case "Cata":
		rbCata.SetChecked(true)
	case "Vanilla":
		rbVanilla.SetChecked(true)
	default:
		rbMop.SetChecked(true)
	}
	refreshExpansionUI()
	mw.Closing().Attach(onClose)

	appendLog("Download settings: " + settingsSource)
	go refreshSettings(context.Background())

	mw.Run()
}
