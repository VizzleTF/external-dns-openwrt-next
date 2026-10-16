package config

import (
	"strings"
	"testing"
)

type nested struct {
	Name  string `mapstructure:"name"`
	Debug bool   `mapstructure:"debug"`
	Port  int    `mapstructure:"port"`
}

type root struct {
	Inner *nested `mapstructure:"inner"`
	ID    string  `mapstructure:"id" required:"true"`
}

func TestReadFillsLeavesFromTheEnvironment(t *testing.T) {
	t.Setenv("INNER_NAME", "router")
	t.Setenv("INNER_DEBUG", "true")
	t.Setenv("INNER_PORT", "8443")
	t.Setenv("ID", "homelab")

	cfg := &root{}
	if err := Read(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Inner == nil || *cfg.Inner != (nested{Name: "router", Debug: true, Port: 8443}) || cfg.ID != "homelab" {
		t.Errorf("got %+v, inner %+v", cfg, cfg.Inner)
	}
}

func TestUnsetKeepsTheDefault(t *testing.T) {
	t.Setenv("ID", "x")

	cfg := &root{Inner: &nested{Name: "default", Port: 443}}
	if err := Read(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Inner.Name != "default" || cfg.Inner.Port != 443 {
		t.Errorf("defaults lost: %+v", cfg.Inner)
	}
}

func TestRequiredMustBeSet(t *testing.T) {
	err := Read(&root{ID: "preset"})
	if err == nil || !strings.Contains(err.Error(), "ID is not set") {
		t.Errorf("got %v, want an error naming ID", err)
	}
}

func TestRequiredAcceptsAnEmptyValue(t *testing.T) {
	t.Setenv("ID", "")

	cfg := &root{ID: "preset"}
	if err := Read(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ID != "" {
		t.Errorf("id: got %q, want it empty", cfg.ID)
	}
}

func TestParseErrorsNameTheVariable(t *testing.T) {
	for key, value := range map[string]string{"INNER_DEBUG": "maybe", "INNER_PORT": "443x"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("ID", "")
			t.Setenv(key, value)

			if err := Read(&root{}); err == nil || !strings.HasPrefix(err.Error(), key+":") {
				t.Errorf("got %v, want an error naming %s", err, key)
			}
		})
	}
}

func TestUnsupportedTypeIsReported(t *testing.T) {
	t.Setenv("RATIO", "0.5")

	cfg := &struct {
		Ratio float64 `mapstructure:"ratio"`
	}{}
	if err := Read(cfg); err == nil || !strings.Contains(err.Error(), "unsupported configuration type float64") {
		t.Errorf("got %v", err)
	}
}

func TestReadRejectsANonPointer(t *testing.T) {
	var nilRoot *root
	for name, cfg := range map[string]any{"value": root{}, "nil pointer": nilRoot, "pointer to non-struct": new(int)} {
		if err := Read(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
