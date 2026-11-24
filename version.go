package main

// version.go — refuse to update a folder that holds another expansion's client

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// expectedClientVersion mirrors WowVersionHelpers.ExpansionToStringVersion.
var expectedClientVersion = map[string]string{
	"Cata":    "4.3.4",
	"Mop":     "5.4.8",
	"Vanilla": "1.12.1",
}

var expansionNames = map[string]string{
	"Cata":    "Cataclysm",
	"Mop":     "Mists of Pandaria",
	"Vanilla": "Vanilla",
}

// checkClientVersion mirrors GameUpdater.ExistsCorrectWowClient: if the folder
// already holds a WoW client of a different version, the path points at the
// wrong installation and updating it would overwrite that client and mangle
// its data files. An empty folder is fine.
func checkClientVersion(gameDir, expansion string) error {
	exe := findClientExe(gameDir)
	want := expectedClientVersion[expansion]
	if exe == "" || want == "" {
		return nil
	}
	got, err := fileVersion(filepath.Join(gameDir, exe))
	if err != nil {
		return fmt.Errorf("cannot read the version of %s in %s: %w", exe, gameDir, err)
	}
	if got != want {
		return fmt.Errorf("%s in %s is WoW %s, but %s needs %s.\n\n"+
			"Choose a different folder for %s, or delete this client first.",
			exe, gameDir, got, expansionNames[expansion], want, expansionNames[expansion])
	}
	return nil
}

// fileVersion returns "major.minor.build" of an executable's version
// resource, the same parts .NET FileVersionInfo exposes as
// FileMajorPart.FileMinorPart.FileBuildPart.
func fileVersion(path string) (string, error) {
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil {
		return "", err
	}
	buf := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])); err != nil {
		return "", err
	}
	var fixed *windows.VS_FIXEDFILEINFO
	var n uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&fixed), &n); err != nil {
		return "", err
	}
	if fixed == nil || n == 0 {
		return "", fmt.Errorf("no fixed version info")
	}
	return fmt.Sprintf("%d.%d.%d",
		fixed.FileVersionMS>>16, fixed.FileVersionMS&0xFFFF, fixed.FileVersionLS>>16), nil
}
