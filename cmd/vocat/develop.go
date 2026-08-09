package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"vocat/internal/config"
	"vocat/internal/store"
)

// developerEnabledSettingKey is the app_settings key that gates the entire
// plugin/extension system. When absent the developer mode defaults to off, so
// a fresh install exposes no plugin surface until an operator explicitly turns
// it on with `vocat develop on` and restarts the service.
const developerEnabledSettingKey = "developer.enabled"

// runDevelop handles the hidden `vocat develop on|off` subcommand. It is
// intentionally excluded from printUsage and the interactive menu: the plugin
// system is an opt-in developer surface, and the toggle must be typed in full
// to activate it. The flag is persisted to app_settings and takes effect on
// the next server start (run() reads it before creating the plugin manager).
func runDevelop(args []string, logger *slog.Logger) error {
	if len(args) == 0 {
		return errors.New(`usage: vocat develop <on|off>`)
	}
	enabled, ok := parseDevelopArg(args[0])
	if !ok {
		return fmt.Errorf(`vocat develop: invalid argument %q (expected "on" or "off")`, args[0])
	}

	// Match the menu's env resolution. An operator runs `vocat develop` on the
	// host where the shell has not sourced /etc/vocat/env (a systemd
	// EnvironmentFile, not a shell rc) and VOCAT_DATABASE_PATH is unset, so
	// config.Load() would otherwise resolve a CWD-relative ./data/vocat.db — a
	// different database than /opt/vocat/data/vocat.db the service reads. The
	// flag would then be written to a DB the service never opens, silently.
	loadMenuEnv()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	database, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	payload, err := json.Marshal(map[string]bool{"enabled": enabled})
	if err != nil {
		return fmt.Errorf("encode developer flag: %w", err)
	}
	if err := database.UpsertAppSetting(ctx, store.AppSetting{
		Key:   developerEnabledSettingKey,
		Value: payload,
	}); err != nil {
		return fmt.Errorf("persist developer flag: %w", err)
	}

	if enabled {
		fmt.Printf("开发者模式已开启。重启 vocat 服务后插件功能生效。\n数据库：%s\n", cfg.DatabasePath)
		fmt.Printf("Developer mode enabled. Restart the vocat service for plugins to take effect.\nDatabase: %s\n", cfg.DatabasePath)
	} else {
		fmt.Printf("开发者模式已关闭。重启 vocat 服务后插件功能将停用。\n数据库：%s\n", cfg.DatabasePath)
		fmt.Printf("Developer mode disabled. Restart the vocat service to deactivate plugins.\nDatabase: %s\n", cfg.DatabasePath)
	}
	return nil
}

func parseDevelopArg(arg string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "on", "1", "true", "yes":
		return true, true
	case "off", "0", "false", "no":
		return false, true
	default:
		return false, false
	}
}

// isDeveloperEnabled reads the persisted developer-mode flag. A missing record
// or an unparseable value resolves to false — the system defaults closed, so
// any read failure keeps plugins off rather than exposing them by accident.
func isDeveloperEnabled(ctx context.Context, database *store.Store) bool {
	setting, err := database.AppSetting(ctx, developerEnabledSettingKey)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "vocat: read developer flag failed; plugin system stays off: %v\n", err)
		}
		return false
	}
	var document struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(setting.Value, &document); err != nil {
		return false
	}
	return document.Enabled
}
