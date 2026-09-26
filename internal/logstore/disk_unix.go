//go:build !windows

package logstore

import "syscall"

// freeBytes 返回该路径所在文件系统的可用字节数。
//
// 为什么日志库要关心磁盘：分片日志、元数据库、系统日志通常共用同一块盘。
// 日志能在保留期内把盘写满，写满之后**同一个进程里的登录、配置发布也会跟着失败** ——
// 故障表现会跑到完全无关的地方去（"登录页 500"），排查成本极高。
// 所以容量控制不只是"删老日志"，还包括"在磁盘见底之前就让日志腾地方"。
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
