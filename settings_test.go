//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Налаштування служби переписуються на ходу: після кожного запису файл має
// читатися цілим, а тимчасовий — зникати.
func TestSaveConfigReplacesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ProgramData", dir)
	old := configSDDL
	configSDDL = "D:P(A;;FA;;;WD)" // у тесті файл має читати звичайний користувач
	defer func() { configSDDL = old }()

	first := svcConfig{Password: "abc12345", Port: 5912, Scale: 100, Area: "all"}
	if err := saveConfig(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Scale, second.Area, second.Mode = 50, "left", "1920x1200"
	if err := saveConfig(second); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got != second {
		t.Fatalf("прочитано %+v, записано %+v", got, second)
	}
	if _, err := os.Stat(configPath() + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("тимчасовий файл лишився")
	}
}

func TestLoadSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("SIMPLEVNC_SETTINGS", p)
	if s := loadSettings(); s != (savedSettings{}) {
		t.Fatalf("без файла налаштування мають бути порожні, а не %+v", s)
	}
	os.WriteFile(p, []byte(`{"scale":66,"area":"m2","mode":"1920x1200"}`), 0o600)
	if s := loadSettings(); s != (savedSettings{Scale: 66, Area: "m2", Mode: "1920x1200"}) {
		t.Fatalf("прочитано %+v", s)
	}
	os.WriteFile(p, []byte(`{зіпсовано`), 0o600)
	if s := loadSettings(); s != (savedSettings{}) {
		t.Fatalf("зіпсований файл має ігноруватися, а не давати %+v", s)
	}
}
