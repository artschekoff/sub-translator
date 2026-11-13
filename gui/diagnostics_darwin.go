//go:build darwin

package main

/*
#cgo LDFLAGS: -framework ApplicationServices
#include <ApplicationServices/ApplicationServices.h>
*/
import "C"

func accessibilityTrusted() bool {
	return C.AXIsProcessTrustedWithOptions(C.CFDictionaryRef(0)) != 0
}
