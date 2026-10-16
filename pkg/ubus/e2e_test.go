//go:build e2e

package ubus

import (
	"context"
	"errors"
	"os"
	"testing"
)

// The router hack/e2e.sh sets up: root, and a user whose ACL grants uci get,
// add, set, delete and apply on dhcp alone.
func e2eClient(t *testing.T, username, password string) Client {
	t.Helper()
	host := os.Getenv("OPENWRT_E2E_HOST")
	if host == "" {
		t.Skip("OPENWRT_E2E_HOST is not set, run hack/e2e.sh")
	}

	config := DefaultConfig()
	config.Hostname = host
	config.Port = 80
	config.SSL = false
	config.Auth = Auth{Username: username, Password: password}
	return New(config, discard)
}

func e2eRoot(t *testing.T) Client {
	return e2eClient(t, "root", os.Getenv("OPENWRT_E2E_ROOT_PASSWORD"))
}

func e2eUser(t *testing.T) Client {
	return e2eClient(t, os.Getenv("OPENWRT_E2E_USERNAME"), os.Getenv("OPENWRT_E2E_PASSWORD"))
}

type dhcpValues struct {
	Values map[string]map[string]any `json:"values"`
}

func getDhcp(t *testing.T, client Client) map[string]map[string]any {
	t.Helper()
	var data dhcpValues
	if err := client.Call(context.Background(), "uci", "get", map[string]string{"config": "dhcp"}, &data); err != nil {
		t.Fatal(err)
	}
	return data.Values
}

// addDomain stages a dhcp domain section and returns its name.
func addDomain(t *testing.T, client Client, name, ip string) string {
	t.Helper()
	var data struct {
		Section string `json:"section"`
	}
	args := map[string]any{"config": "dhcp", "type": "domain", "values": map[string]string{"name": name, "ip": ip}}
	if err := client.Call(context.Background(), "uci", "add", args, &data); err != nil {
		t.Fatal(err)
	}
	if data.Section == "" {
		t.Fatal("uci add returned no section")
	}
	return data.Section
}

func apply(client Client) error {
	return client.Call(context.Background(), "uci", "apply", map[string]bool{"rollback": false}, nil)
}

func TestE2ELogin(t *testing.T) {
	for name, client := range map[string]Client{"root": e2eRoot(t), "dedicated user": e2eUser(t)} {
		t.Run(name, func(t *testing.T) {
			found := false
			for _, section := range getDhcp(t, client) {
				found = found || section[".type"] == "dnsmasq"
			}
			if !found {
				t.Error("uci get dhcp holds no dnsmasq section")
			}
		})
	}
}

func TestE2EWrongPassword(t *testing.T) {
	client := e2eClient(t, os.Getenv("OPENWRT_E2E_USERNAME"), "wrong-password")
	err := client.Call(context.Background(), "uci", "get", map[string]string{"config": "dhcp"}, nil)
	if !errors.Is(err, Status(6)) {
		t.Errorf("got %v, want UBUS_STATUS_PERMISSION_DENIED", err)
	}
}

func TestE2EAddSetDeleteApply(t *testing.T) {
	client := e2eUser(t)
	ctx := context.Background()

	section := addDomain(t, client, "e2e-roundtrip.lan", "10.9.9.1")
	t.Cleanup(func() {
		root := e2eRoot(t)
		if root.Call(ctx, "uci", "delete", map[string]string{"config": "dhcp", "section": section}, nil) == nil {
			_ = apply(root)
		}
	})
	set := map[string]any{"config": "dhcp", "section": section, "values": map[string]string{"ip": "10.9.9.2"}}
	if err := client.Call(ctx, "uci", "set", set, nil); err != nil {
		t.Fatal(err)
	}
	if err := apply(client); err != nil {
		t.Fatal(err)
	}

	// A fresh session sees only what was committed.
	if got := getDhcp(t, e2eRoot(t))[section]; got["name"] != "e2e-roundtrip.lan" || got["ip"] != "10.9.9.2" {
		t.Errorf("committed section: got %v", got)
	}

	del := map[string]string{"config": "dhcp", "section": section}
	if err := client.Call(ctx, "uci", "delete", del, nil); err != nil {
		t.Fatal(err)
	}
	if err := apply(client); err != nil {
		t.Fatal(err)
	}
	if _, ok := getDhcp(t, e2eRoot(t))[section]; ok {
		t.Error("deleted section is still there")
	}

	if err := client.Call(ctx, "uci", "set", set, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("set on a deleted section: got %v, want ErrNotFound", err)
	}
	if err := client.Call(ctx, "uci", "delete", del, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete of a deleted section: got %v, want ErrNotFound", err)
	}
	if err := apply(client); !errors.Is(err, ErrNoData) {
		t.Errorf("apply with nothing staged: got %v, want ErrNoData", err)
	}
}

func TestE2EResetRevertsWhatWasStaged(t *testing.T) {
	client := e2eUser(t)

	section := addDomain(t, client, "e2e-reset.lan", "10.9.9.3")
	if _, ok := getDhcp(t, client)[section]; !ok {
		t.Fatal("the session does not see what it staged")
	}

	client.Reset()
	if err := apply(client); !errors.Is(err, ErrNoData) {
		t.Errorf("apply after Reset: got %v, want ErrNoData", err)
	}
	if _, ok := getDhcp(t, e2eRoot(t))[section]; ok {
		t.Error("the abandoned session's section was committed")
	}
}

func TestE2EDedicatedUserIsConfinedToDhcp(t *testing.T) {
	client := e2eUser(t)
	ctx := context.Background()

	// The ubus ACL lets the call through, the uci ACL refuses the config.
	if err := client.Call(ctx, "uci", "get", map[string]string{"config": "network"}, nil); !errors.Is(err, Status(6)) {
		t.Errorf("uci get network: got %v, want UBUS_STATUS_PERMISSION_DENIED", err)
	}
	add := map[string]any{"config": "network", "type": "interface", "values": map[string]string{"proto": "none"}}
	if err := client.Call(ctx, "uci", "add", add, nil); !errors.Is(err, Status(6)) {
		t.Errorf("uci add network: got %v, want UBUS_STATUS_PERMISSION_DENIED", err)
	}

	// Methods outside the ubus ACL are refused before they reach ubus.
	for _, call := range []struct{ object, method string }{{"file", "exec"}, {"rc", "init"}, {"uci", "changes"}} {
		var rerr *rpcError
		err := client.Call(ctx, call.object, call.method, nil, nil)
		if !errors.As(err, &rerr) || rerr.Code != codeAccessDenied {
			t.Errorf("%s %s: got %v, want access denied", call.object, call.method, err)
		}
	}
}
