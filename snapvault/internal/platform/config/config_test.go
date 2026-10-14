package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMustString_readsFileVariant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SNAPVAULT_TEST_SECRET", "from-env")
	t.Setenv("SNAPVAULT_TEST_SECRET_FILE", path)

	// The mounted file wins over the plain variable, as in FileOrEnv.
	if got := MustString("SNAPVAULT_TEST_SECRET"); got != "from-file" {
		t.Fatalf("MustString = %q, want %q", got, "from-file")
	}
}

func TestMustString_fallsBackToEnv(t *testing.T) {
	t.Setenv("SNAPVAULT_TEST_SECRET", "from-env")
	if got := MustString("SNAPVAULT_TEST_SECRET"); got != "from-env" {
		t.Fatalf("MustString = %q, want %q", got, "from-env")
	}
}

func TestMustString_panicsWhenUnset(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustString did not panic for an unset key")
		}
	}()
	MustString("SNAPVAULT_TEST_DEFINITELY_UNSET")
}
