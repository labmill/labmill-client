//go:build !windows && !darwin

package keepawake

func osStart() func() { return func() {} }
