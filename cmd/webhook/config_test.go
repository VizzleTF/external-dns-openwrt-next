package main

import (
	"strings"
	"testing"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/config"
)

// These are the variable names real deployments set. The config layer was
// rewritten from viper to a small reflection binder, so the exact spelling is
// pinned here: a rename would otherwise start the provider silently on
// defaults, pointing at the wrong router.
func TestEnvironmentVariableNames(t *testing.T) {
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "42")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_ENCODING", "console")
	t.Setenv("ROUTER_ADDRESS", "0.0.0.0")
	t.Setenv("ROUTER_PORT", "9999")
	t.Setenv("ROUTER_HEALTHCHECK_PORT", "9090")
	// Removed in v0.9.0. A deployment that still sets it has to start.
	t.Setenv("PROVIDER_OPENWRT_RELOADSTRATEGY", "restart")
	t.Setenv("PROVIDER_OPENWRT_OWNERSHIPID", "homelab")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_HOSTNAME", "10.0.0.1")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_PORT", "8443")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_SSL", "false")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_TIMEOUT", "30")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_INSECURE_SKIP_VERIFY", "true")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_AUTH_USERNAME", "root")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_AUTH_PASSWORD", "secret")
	t.Setenv("PROVIDER_STATUS_ENABLED", "true")
	t.Setenv("PROVIDER_STATUS_NAMESPACE", "dns")

	cfg := defaultConfig()
	if err := config.Read(cfg); err != nil {
		t.Fatalf("read config: %v", err)
	}

	rpc := cfg.Provider.OpenWRT.Ubus

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"SHUTDOWN_TIMEOUT_SECONDS", cfg.ShutdownTimeout, 42},
		{"LOG_LEVEL", cfg.Log.Level, "debug"},
		{"LOG_ENCODING", cfg.Log.Encoding, "console"},
		{"ROUTER_ADDRESS", cfg.Router.Address, "0.0.0.0"},
		{"ROUTER_PORT", cfg.Router.Port, "9999"},
		{"ROUTER_HEALTHCHECK_PORT", cfg.Router.HealthCheckPort, "9090"},
		{"PROVIDER_OPENWRT_OWNERSHIPID", cfg.Provider.OpenWRT.OwnershipID, "homelab"},
		{"PROVIDER_OPENWRT_LUCIRPC_HOSTNAME", rpc.Hostname, "10.0.0.1"},
		{"PROVIDER_OPENWRT_LUCIRPC_PORT", rpc.Port, 8443},
		{"PROVIDER_OPENWRT_LUCIRPC_SSL", rpc.SSL, false},
		{"PROVIDER_OPENWRT_LUCIRPC_TIMEOUT", rpc.Timeout, 30},
		{"PROVIDER_OPENWRT_LUCIRPC_INSECURE_SKIP_VERIFY", rpc.InsecureSkipVerify, true},
		{"PROVIDER_OPENWRT_LUCIRPC_AUTH_USERNAME", rpc.Auth.Username, "root"},
		{"PROVIDER_OPENWRT_LUCIRPC_AUTH_PASSWORD", rpc.Auth.Password, "secret"},
		{"PROVIDER_STATUS_ENABLED", cfg.Provider.Status.Enabled, true},
		{"PROVIDER_STATUS_NAMESPACE", cfg.Provider.Status.Namespace, "dns"},
	}

	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s: got %v, want %v", check.name, check.got, check.want)
		}
	}
}

func TestDefaultsSurviveAnEmptyEnvironment(t *testing.T) {
	// The one variable without a default.
	t.Setenv("PROVIDER_OPENWRT_OWNERSHIPID", "homelab")

	cfg := defaultConfig()
	if err := config.Read(cfg); err != nil {
		t.Fatalf("read config: %v", err)
	}

	if cfg.Router.Port != "8888" {
		t.Errorf("router port: got %q, want 8888", cfg.Router.Port)
	}
	if cfg.Router.Address != "127.0.0.1" {
		t.Errorf("router address: got %q, want 127.0.0.1", cfg.Router.Address)
	}
	if cfg.Router.HealthCheckPort != "8080" {
		t.Errorf("healthcheck port: got %q, want 8080", cfg.Router.HealthCheckPort)
	}
}

func TestOwnershipIDIsRequired(t *testing.T) {
	// Unset would otherwise mean ownership off, and policy=sync then deletes
	// every record on the router the cluster does not ask for.
	err := config.Read(defaultConfig())
	if err == nil || !strings.Contains(err.Error(), "PROVIDER_OPENWRT_OWNERSHIPID") {
		t.Fatalf("got %v, want an error naming PROVIDER_OPENWRT_OWNERSHIPID", err)
	}
}

func TestAnEmptyValueIsNotTheSameAsUnset(t *testing.T) {
	// An empty value overwrites the default rather than being ignored as
	// unset: OWNERSHIPID="" is how ownership is switched off explicitly.
	t.Setenv("PROVIDER_OPENWRT_OWNERSHIPID", "")

	cfg := defaultConfig()
	cfg.Provider.OpenWRT.OwnershipID = "preset"
	if err := config.Read(cfg); err != nil {
		t.Fatalf("read config: %v", err)
	}

	if cfg.Provider.OpenWRT.OwnershipID != "" {
		t.Errorf("ownership id: got %q, want it empty", cfg.Provider.OpenWRT.OwnershipID)
	}
}

func TestInvalidNumberIsReported(t *testing.T) {
	t.Setenv("PROVIDER_OPENWRT_OWNERSHIPID", "")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_PORT", "not-a-number")

	if err := config.Read(defaultConfig()); err == nil {
		t.Fatal("expected an error for a non-numeric port")
	}
}
