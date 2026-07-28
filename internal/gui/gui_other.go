//go:build !windows && !darwin

package gui

import "errors"

func Run(_, _ []byte) error { return errors.New("当前 GUI 版本仅支持 Windows") }

func RunClient(_ []byte) error { return errors.New("当前客户端仅支持 Windows") }
