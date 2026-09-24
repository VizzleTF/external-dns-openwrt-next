// Package config fills a configuration struct from the environment.
//
// This replaces viper, which dragged in HCL, TOML, INI and properties parsers
// plus a filesystem abstraction to read configuration that only ever comes from
// environment variables.
//
// Keys are derived from `mapstructure` tags: the path from the root struct is
// joined with "_" and upper-cased, so
//
//	provider.openwrt.lucirpc.hostname -> PROVIDER_OPENWRT_LUCIRPC_HOSTNAME
//
// which is exactly the naming viper produced. Unset variables leave the
// existing (default) value untouched; unknown variables are ignored.
package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
)

// Read populates config, which must be a non-nil pointer to a struct.
func Read(config any) error {
	value := reflect.ValueOf(config)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return fmt.Errorf("config must be a non-nil pointer, got %T", config)
	}

	if value.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("config must point to a struct, got %T", config)
	}

	return bind(value.Elem(), nil)
}

func bind(structValue reflect.Value, path []string) error {
	structType := structValue.Type()

	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		if !field.IsExported() {
			continue
		}

		fieldPath := append(append([]string{}, path...), field.Tag.Get("mapstructure"))

		if err := bindField(structValue.Field(i), fieldPath); err != nil {
			return err
		}
	}

	return nil
}

func bindField(value reflect.Value, path []string) error {
	switch value.Kind() {
	case reflect.Pointer:
		if value.Type().Elem().Kind() != reflect.Struct {
			return nil
		}
		if value.IsNil() {
			value.Set(reflect.New(value.Type().Elem()))
		}
		return bind(value.Elem(), path)

	case reflect.Struct:
		return bind(value, path)

	default:
		return setLeaf(value, path)
	}
}

func setLeaf(value reflect.Value, path []string) error {
	key := envKey(path)
	raw, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}

	switch value.Kind() {
	case reflect.String:
		value.SetString(raw)

	case reflect.Bool:
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		value.SetBool(parsed)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		value.SetInt(parsed)

	default:
		return fmt.Errorf("%s: unsupported configuration type %s", key, value.Kind())
	}

	return nil
}

// envKey renders the environment variable name for a field path.
func envKey(path []string) string {
	return strings.ToUpper(strings.Join(path, "_"))
}
