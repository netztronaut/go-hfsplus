//go:build !darwin

package main

import "errors"

func runMeasure(work, testdata string, u *ucd) error {
	return errors.New("-measure needs macOS (hdiutil and the HFS+ kernel extension)")
}
