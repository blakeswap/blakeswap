//go:build !darwin && !linux

package storage

func cloneFile(string, string) error { return ErrCloneUnsupported }
