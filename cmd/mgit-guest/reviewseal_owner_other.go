//go:build !linux

package main

import "errors"

func ownedByRootWithGroup(string, int) error {
	return errors.New("the review seal is only available in a Linux guest")
}
