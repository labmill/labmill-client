//go:build windows

package autostart

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func enable(e Entry) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(e.Name, windowsCommand(e)); err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	return nil
}

func disable(e Entry) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	defer k.Close()
	if err := k.DeleteValue(e.Name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("autostart: %w", err)
	}
	return nil
}

func enabled(e Entry) (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer k.Close()
	_, _, err = k.GetValue(e.Name, nil) // any command counts, even an edited one
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
