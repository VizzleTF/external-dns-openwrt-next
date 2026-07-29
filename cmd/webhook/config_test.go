package main

import (
	"testing"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/config"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/openwrt"
)

// These are the variable names real deployments set. The config layer was
// rewritten from viper to a small reflection binder, so the exact spelling is
// pinned here: a rename would otherwise start the provider silently on
// defaults, pointing at the wrong router.
func TestEnvironmentVariableNames(t *testing.T) {
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "42")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_ENCODING", "console")
	t.Setenv("ROUTER_PORT", "9999")
	t.Setenv("ROUTER_HEALTHCHECK_PATH", "/healthz")
	t.Setenv("PROVIDER_OPENWRT_RELOADSTRATEGY", openwrt.ReloadStrategyReload)
	t.Setenv("PROVIDER_OPENWRT_OWNERSHIPID", "homelab")
	t.Setenv("PROVIDER_OPENWRT_OWNERSHIPOPTION", "marker")
	t.Setenv("PROVIDER_OPENWRT_ADOPTEXISTING", "false")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_HOSTNAME", "10.0.0.1")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_PORT", "8443")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_SSL", "false")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_RPC_ID", "7")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_TIMEOUT", "30")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_INSECURE_SKIP_VERIFY", "true")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_AUTH_USERNAME", "root")
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_AUTH_PASSWORD", "secret")

	cfg := defaultConfig()
	if err := config.Read(cfg); err != nil {
		t.Fatalf("read config: %v", err)
	}

	rpc := cfg.Provider.OpenWRT.LuciRPC

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"SHUTDOWN_TIMEOUT_SECONDS", cfg.ShutdownTimeout, 42},
		{"LOG_LEVEL", cfg.Log.Level, "debug"},
		{"LOG_ENCODING", cfg.Log.Encoding, "console"},
		{"ROUTER_PORT", cfg.Router.Port, "9999"},
		{"ROUTER_HEALTHCHECK_PATH", cfg.Router.HealthCheckPath, "/healthz"},
		{"PROVIDER_OPENWRT_RELOADSTRATEGY", cfg.Provider.OpenWRT.ReloadStrategy, openwrt.ReloadStrategyReload},
		{"PROVIDER_OPENWRT_OWNERSHIPID", cfg.Provider.OpenWRT.OwnershipID, "homelab"},
		{"PROVIDER_OPENWRT_OWNERSHIPOPTION", cfg.Provider.OpenWRT.OwnershipOption, "marker"},
		{"PROVIDER_OPENWRT_ADOPTEXISTING", cfg.Provider.OpenWRT.AdoptExisting, false},
		{"PROVIDER_OPENWRT_LUCIRPC_HOSTNAME", rpc.Hostname, "10.0.0.1"},
		{"PROVIDER_OPENWRT_LUCIRPC_PORT", rpc.Port, 8443},
		{"PROVIDER_OPENWRT_LUCIRPC_SSL", rpc.SSL, false},
		{"PROVIDER_OPENWRT_LUCIRPC_RPC_ID", rpc.RpcID, 7},
		{"PROVIDER_OPENWRT_LUCIRPC_TIMEOUT", rpc.Timeout, 30},
		{"PROVIDER_OPENWRT_LUCIRPC_INSECURE_SKIP_VERIFY", rpc.InsecureSkipVerify, true},
		{"PROVIDER_OPENWRT_LUCIRPC_AUTH_USERNAME", rpc.Auth.Username, "root"},
		{"PROVIDER_OPENWRT_LUCIRPC_AUTH_PASSWORD", rpc.Auth.Password, "secret"},
	}

	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s: got %v, want %v", check.name, check.got, check.want)
		}
	}
}

func TestDefaultsSurviveAnEmptyEnvironment(t *testing.T) {
	cfg := defaultConfig()
	if err := config.Read(cfg); err != nil {
		t.Fatalf("read config: %v", err)
	}

	if cfg.Router.Port != "8888" {
		t.Errorf("router port: got %q, want 8888", cfg.Router.Port)
	}
	if cfg.Provider.OpenWRT.ReloadStrategy != openwrt.ReloadStrategyRestart {
		t.Errorf("reload strategy: got %q", cfg.Provider.OpenWRT.ReloadStrategy)
	}
	if cfg.Provider.OpenWRT.OwnershipID != "" {
		t.Errorf("ownership must stay off by default, got %q", cfg.Provider.OpenWRT.OwnershipID)
	}
	if !cfg.Provider.OpenWRT.AdoptExisting {
		t.Error("adoption must default to true")
	}
}

func TestInvalidNumberIsReported(t *testing.T) {
	t.Setenv("PROVIDER_OPENWRT_LUCIRPC_PORT", "not-a-number")

	if err := config.Read(defaultConfig()); err == nil {
		t.Fatal("expected an error for a non-numeric port")
	}
}
