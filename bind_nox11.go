//go:build !windows && !(linux && cgo)

package main

func x11Keys(map[string]bool) {}
func x11KeyName(int) string   { return "" }

const haveKeyboard = false
