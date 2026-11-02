package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const configFile = "settings.json"

// Config is everything this app remembers between runs.
//
// Deliberately small, and deliberately not a password. The window signs in the
// way a browser does and the session lives in the WebView2 profile, which
// Windows encrypts per user; nothing here is worth stealing on its own.
type Config struct {
	// Server is the base URL, already normalised.
	Server string `json:"server"`

	// Device is a long-lived token that lets the agent hold the messaging
	// socket open while no window exists, so a call can ring on a machine
	// where nothing is running. Empty until the signed-in window issues one.
	Device string `json:"device,omitempty"`

	// StartAtLogin mirrors the registry entry, so the menu can show a state
	// without reading the registry on every paint.
	StartAtLogin bool `json:"startAtLogin,omitempty"`

	// Permissions is what was answered on first run (see
	// permissions_windows.go). Nil until asked.
	Permissions *Permissions `json:"permissions,omitempty"`
}

// configDir is %APPDATA%\Aether, created if it is not there.
func configDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, appName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	return dir, nil
}

func loadConfig(dir string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(filepath.Join(dir, configFile))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("settings are not readable: %w", err)
	}
	return c, nil
}

// saveConfig writes atomically, because a settings file half-written by a
// machine that lost power is a program that will not start again.
func saveConfig(dir string, c Config) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, configFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, configFile))
}
