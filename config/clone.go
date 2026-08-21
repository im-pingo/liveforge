package config

import "reflect"

// CloneConfig returns a deep copy suitable for transferring config ownership
// across manager, server, and caller boundaries.
func CloneConfig(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	return cloneConfigValue(reflect.ValueOf(cfg)).Interface().(*Config)
}

func cloneConfigValue(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.New(value.Type().Elem())
		cloned.Elem().Set(cloneConfigValue(value.Elem()))
		return cloned
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.New(value.Type()).Elem()
		cloned.Set(cloneConfigValue(value.Elem()))
		return cloned
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			cloned.Index(i).Set(cloneConfigValue(value.Index(i)))
		}
		return cloned
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			cloned.SetMapIndex(cloneConfigValue(iterator.Key()), cloneConfigValue(iterator.Value()))
		}
		return cloned
	case reflect.Struct:
		cloned := reflect.New(value.Type()).Elem()
		cloned.Set(value)
		for i := 0; i < value.NumField(); i++ {
			if cloned.Field(i).CanSet() {
				cloned.Field(i).Set(cloneConfigValue(value.Field(i)))
			}
		}
		return cloned
	default:
		return value
	}
}
