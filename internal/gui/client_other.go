//go:build !windows && !darwin && client

package gui

import "errors"

func Run(_, _ []byte) error { return errors.New("server entry point is unavailable in client builds") }

func RunClient(_ []byte) error { return errors.New("desktop client is not supported on this platform") }
