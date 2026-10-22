//go:build !unix

package peercred

func groupAlive(int) bool { return false }
