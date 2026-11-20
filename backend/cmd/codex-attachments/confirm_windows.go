package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func isReparsePoint(fi os.FileInfo) bool {
	data, _ := fi.Sys().(*syscall.Win32FileAttributeData)
	return fi.Mode()&os.ModeSymlink != 0 || (data != nil && data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0)
}

func isLocalVolume(path string) bool {
	name := filepath.VolumeName(path)
	if len(name) != 2 || name[1] != ':' {
		return false
	}
	volume, _ := syscall.UTF16PtrFromString(name + "\\")
	drive := windows.GetDriveType(volume)
	return drive == 2 || drive == 3 || drive == 5 || drive == 6 // exclude network and unknown drives
}

func confirmFiles(destination string, paths []string) bool {
	caption, _ := syscall.UTF16PtrFromString("Codex：确认发送附件")
	msg, err := syscall.UTF16PtrFromString("把以下文件以只读方式发送到模型线路？\n\n" +
		strings.Join(paths, "\n") + "\n\n目的地：" + destination + "/v1\n\n选择「否」不会读取文件。")
	if err != nil {
		return false
	}
	const yesNoDefaultNo = 0x4 | 0x20 | 0x100 | 0x10000 | 0x40000
	result, _, _ := windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW").Call(
		0, uintptr(unsafe.Pointer(msg)), uintptr(unsafe.Pointer(caption)), yesNoDefaultNo)
	runtime.KeepAlive(msg)
	runtime.KeepAlive(caption)
	return result == 6 // IDYES
}
