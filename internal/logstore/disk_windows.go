//go:build windows

package logstore

import (
	"syscall"
	"unsafe"
)

// freeBytes 返回该路径所在卷的可用字节数（Windows 实现）。
//
// Windows 只是开发/验收环境（生产是 Linux），但保留实现的价值在于：
// 容量控制的逻辑与测试可以跨平台跑，不必给磁盘告警写一条"仅 Linux 生效"的分支 ——
// 那种分支在开发机上永远不会被执行，等上生产才第一次跑。
func freeBytes(path string) (int64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var freeAvail, total, totalFree uint64
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetDiskFreeSpaceExW")
	r, _, e := proc.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeAvail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 {
		return 0, e
	}
	return int64(freeAvail), nil
}
