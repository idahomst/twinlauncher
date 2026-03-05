package main

// gui.go — Win32 GUI built with lxn/walk

import (
	"fmt"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

// ── Download state shared between worker goroutine and UI ────────────────────

var (
	dlRunning int32 // atomic: 1 while download is in progress
	dlCancel  int32 // atomic: set to 1 to request cancellation
)

// Walk widget references (set during window creation)
var (
	mw        *walk.MainWindow
	cbExp     *walk.ComboBox
	lePath    *walk.LineEdit
	cbRL      *walk.ComboBox
	chkClr    *walk.CheckBox
	chkSetRL  *walk.CheckBox
	btnUpdate *walk.PushButton
	btnPlay   *walk.PushButton
	pb        *walk.ProgressBar
	lblStatus *walk.Label
	teLog     *walk.TextEdit
)

// ── UI update helpers (safe to call from any goroutine) ──────────────────────

func logLine(s string) {
	mw.Synchronize(func() {
		appendLog(s)
	})
}

func setStatus(s string) {
	mw.Synchronize(func() {
		lblStatus.SetText(s)
	})
}

func setPercent(p int) {
	mw.Synchronize(func() {
		pb.SetValue(p)
	})
}

const emGetTextLength = 0x000E // WM_GETTEXTLENGTH (not exposed by lxn/win)

// appendLog appends a line to the log TextEdit efficiently using Win32
// EM_SETSEL / EM_REPLACESEL so we never have to read back the whole text.
func appendLog(line string) {
	hwnd := teLog.Handle()
	l := win.SendMessage(hwnd, emGetTextLength, 0, 0)
	win.SendMessage(hwnd, win.EM_SETSEL, l, l)
	text := line + "\r\n"
	p, _ := syscall.UTF16PtrFromString(text)
	win.SendMessage(hwnd, win.EM_REPLACESEL, 0, uintptr(unsafe.Pointer(p)))
}

// ── Shell browse-for-folder (syscall, avoids lxn/win struct layout concerns) ─

var (
	shell32dll        = syscall.NewLazyDLL("shell32.dll")
	ole32dll          = syscall.NewLazyDLL("ole32.dll")
	_SHBrowseForFolder = shell32dll.NewProc("SHBrowseForFolderW")
	_SHGetPathFromIDList = shell32dll.NewProc("SHGetPathFromIDListW")
	_CoTaskMemFree     = ole32dll.NewProc("CoTaskMemFree")
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

	// MessageBox flags (used by main.go)
	MB_OK        uint32 = 0x0000
	MB_ICONERROR uint32 = 0x0010
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

// ── msgBox (needed by main.go before mw is initialised) ─────────────────────

func msgBox(_ uintptr, title, text string, flags uint32) {
	// walk.MsgBox handles nil owner safely (uses HWND 0)
	walk.MsgBox(mw, title, text, walk.MsgBoxStyle(flags))
}

// ── Button handlers ───────────────────────────────────────────────────────────

func onExpansionChanged() {
	if idx := cbExp.CurrentIndex(); idx >= 0 {
		exps := appCfg.AppSettings.ExpansionSelectionSettings.AvailableExpansions
		if idx < len(exps) {
			userCfg.Expansion = exps[idx]
		}
	}
	saveUserSettings()
	lePath.SetText(gamePath())
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

func onUpdate() {
	// If already running — cancel
	if atomic.LoadInt32(&dlRunning) == 1 {
		atomic.StoreInt32(&dlCancel, 1)
		btnUpdate.SetText("Cancelling…")
		return
	}

	// Read current control values
	if idx := cbExp.CurrentIndex(); idx >= 0 {
		exps := appCfg.AppSettings.ExpansionSelectionSettings.AvailableExpansions
		if idx < len(exps) {
			userCfg.Expansion = exps[idx]
		}
	}
	setGamePath(lePath.Text())
	userCfg.Realmlist = cbRL.Text()
	userCfg.ClearCache = chkClr.Checked()
	userCfg.SkipRealmlistSetup = !chkSetRL.Checked()
	saveUserSettings()

	gp := gamePath()
	if gp == "" {
		walk.MsgBox(mw, "Error", "Please set the game path first.", walk.MsgBoxIconError|walk.MsgBoxOK)
		return
	}

	// Prepare state
	atomic.StoreInt32(&dlRunning, 1)
	atomic.StoreInt32(&dlCancel, 0)
	setPercent(0)
	setStatus("Starting…")
	btnUpdate.SetText("Cancel")
	btnPlay.SetEnabled(false)

	logLine(fmt.Sprintf("=== Update %s in %s ===", userCfg.Expansion, gp))

	go func() {
		var err error
		switch userCfg.Expansion {
		case "Cata":
			err = updateCata(gp)
		case "Mop":
			err = updateMop(gp)
		case "Vanilla":
			err = updateVanilla(gp)
		default:
			err = fmt.Errorf("unknown expansion %q", userCfg.Expansion)
		}
		atomic.StoreInt32(&dlRunning, 0)
		mw.Synchronize(func() {
			btnUpdate.SetText("Check && Update")
			btnPlay.SetEnabled(true)
			if err != nil {
				lblStatus.SetText("Error: " + err.Error())
				appendLog("ERROR: " + err.Error())
				walk.MsgBox(mw, "Update Error", err.Error(), walk.MsgBoxIconError|walk.MsgBoxOK)
			} else {
				lblStatus.SetText("Done!")
				appendLog("=== Update complete! ===")
				pb.SetValue(100)
			}
		})
	}()
}

func onPlay() {
	if idx := cbExp.CurrentIndex(); idx >= 0 {
		exps := appCfg.AppSettings.ExpansionSelectionSettings.AvailableExpansions
		if idx < len(exps) {
			userCfg.Expansion = exps[idx]
		}
	}
	setGamePath(lePath.Text())
	userCfg.Realmlist = cbRL.Text()
	userCfg.ClearCache = chkClr.Checked()
	userCfg.SkipRealmlistSetup = !chkSetRL.Checked()
	saveUserSettings()
	doPlay()
}

// ── Window ───────────────────────────────────────────────────────────────────

func runGUI() {
	exps := appCfg.AppSettings.ExpansionSelectionSettings.AvailableExpansions
	rls := appCfg.AppSettings.Realmlists

	// Find saved indices
	expIdx := 0
	for i, e := range exps {
		if e == userCfg.Expansion {
			expIdx = i
			break
		}
	}
	rlIdx := 0
	rlInList := false
	for i, r := range rls {
		if r == userCfg.Realmlist {
			rlIdx = i
			rlInList = true
			break
		}
	}

	if err := (MainWindow{
		AssignTo: &mw,
		Title:    "twinlauncher",
		MinSize:  Size{Width: 440, Height: 400},
		Size:     Size{Width: 440, Height: 400},
		Layout:   VBox{Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}, Spacing: 6},
		Children: []Widget{
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					Label{Text: "Expansion:", MinSize: Size{Width: 80}},
					ComboBox{
						AssignTo:              &cbExp,
						Model:                 exps,
						CurrentIndex:          expIdx,
						OnCurrentIndexChanged: onExpansionChanged,
					},
					HSpacer{},
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					Label{Text: "Game Path:", MinSize: Size{Width: 80}},
					LineEdit{
						AssignTo: &lePath,
						Text:     gamePath(),
						OnTextChanged: func() {
							setGamePath(lePath.Text())
						},
					},
					PushButton{
						Text:      "…",
						MaxSize:   Size{Width: 32},
						OnClicked: onBrowse,
					},
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					Label{Text: "Realmlist:", MinSize: Size{Width: 80}},
					ComboBox{
						AssignTo:     &cbRL,
						Editable:     true,
						Model:        rls,
						CurrentIndex: rlIdx,
					},
					HSpacer{},
				},
			},
			CheckBox{
				AssignTo: &chkClr,
				Text:     "Clear cache on launch",
				Checked:  userCfg.ClearCache,
			},
			CheckBox{
				AssignTo: &chkSetRL,
				Text:     "Set realmlist before launch",
				Checked:  !userCfg.SkipRealmlistSetup, // inverted: false=skip, so checked=do set
			},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					PushButton{
						AssignTo:  &btnUpdate,
						Text:      "Check && Update",
						OnClicked: onUpdate,
					},
					PushButton{
						AssignTo:  &btnPlay,
						Text:      "Play",
						OnClicked: onPlay,
					},
				},
			},
			ProgressBar{
				AssignTo: &pb,
				MinValue: 0,
				MaxValue: 100,
			},
			Label{
				AssignTo: &lblStatus,
				Text:     "Ready.",
			},
			TextEdit{
				AssignTo: &teLog,
				ReadOnly: true,
				VScroll:  true,
				MinSize:  Size{Height: 100},
			},
		},
	}).Create(); err != nil {
		panic(err)
	}

	if !rlInList && userCfg.Realmlist != "" {
		cbRL.SetText(userCfg.Realmlist)
	}

	mw.Run()
}
