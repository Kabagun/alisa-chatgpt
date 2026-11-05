//go:build !linux

package main

import "errors"

func disableCore() error { return errors.New("linux required") }
