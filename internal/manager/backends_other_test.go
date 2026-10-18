//go:build !linux && !darwin

package manager_test

import "testing"

func platformBackends() []string { return []string{"copy"} }

func usable(*testing.T, string) error { return nil }
