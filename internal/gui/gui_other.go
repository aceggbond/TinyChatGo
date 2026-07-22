//go:build !windows

package gui

import "errors"

func Run() error { return errors.New("当前 GUI 版本仅支持 Windows") }
