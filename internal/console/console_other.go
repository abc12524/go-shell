//go:build !windows

// Package console：非 Windows 平台无需处理控制台编码（终端本来就是 UTF-8）。
package console

// Init 在非 Windows 平台是空操作，返回空的恢复函数以便调用方统一处理。
func Init() func() { return func() {} }
