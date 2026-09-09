package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAuditPersistenceConfigBounds(t *testing.T) {
	for _, value := range []string{"-1", "1", "65535", "1073741825"} {
		cfg := Defaults()
		if err := yaml.Unmarshal([]byte("api:\n  audit:\n    max_bytes: "+value+"\n"), cfg); err != nil {
			t.Fatal(err)
		}
		if err := Validate(cfg); err == nil {
			t.Errorf("accepted api.audit.max_bytes=%s", value)
		}
	}
	for _, value := range []int64{0, 65536, 8388608, 1073741824} {
		cfg := Defaults()
		cfg.API.Audit.MaxBytes = value
		if err := Validate(cfg); err != nil {
			t.Errorf("rejected api.audit.max_bytes=%d: %v", value, err)
		}
	}
	if got := Defaults().API.Audit.MaxBytes; got != 8388608 {
		t.Fatalf("default audit file size = %d", got)
	}
	for _, path := range []string{" ", ".", "..", "/"} {
		cfg := Defaults()
		cfg.API.Audit.Path = path
		if err := Validate(cfg); err == nil {
			t.Errorf("accepted non-file audit path %q", path)
		}
	}
}
