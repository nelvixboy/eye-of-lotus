package config

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// missing is a path that is guaranteed not to exist, so that the tests can
// tell "no configuration" from "a configuration that failed to load".
var missing = filepath.Join("testdata", "does-not-exist.json")

func TestVersionDoesNotNeedAConfiguration(t *testing.T) {
	// The configuration is mode 0600 and owned by the service account, so
	// a plain user asking a binary what version it is must not be told
	// about a permission problem instead.
	_, err := Load([]string{"-config", missing, "-version"})
	if !errors.Is(err, ErrShowVersion) {
		t.Fatalf("Load with -version returned %v, want ErrShowVersion", err)
	}
}

func TestHashPasswordDoesNotNeedAConfiguration(t *testing.T) {
	_, err := Load([]string{"-config", missing, "-hash-password", "secret"})
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("Load with -hash-password returned %v, want ErrHandled", err)
	}
}

func TestExplicitConfigurationMustExist(t *testing.T) {
	// Other flags do need the file, and naming one that is not there is a
	// mistake worth reporting rather than quietly falling back to
	// defaults.
	_, err := Load([]string{"-config", missing, "-listen", "127.0.0.1:1"})
	if err == nil {
		t.Fatal("an explicitly named missing configuration should be an error")
	}
}

func TestPasswordHashing(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !IsHashed(hash) {
		t.Errorf("the result is not recognised as a hash: %q", hash)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Error("the right password was rejected")
	}
	if VerifyPassword(hash, "correct horse battery stapler") {
		t.Error("a wrong password was accepted")
	}
	if VerifyPassword(hash, "") {
		t.Error("an empty password was accepted")
	}
	// A plain value still works, which is what makes a first run possible,
	// but it is not mistaken for a hash.
	if !VerifyPassword("plain", "plain") {
		t.Error("a plain password was rejected")
	}
	if IsHashed("plain") {
		t.Error("a plain password was reported as a hash")
	}
	// Two hashes of the same password differ, because the salt is random.
	second, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if hash == second {
		t.Error("hashing the same password twice produced the same value, so the salt is not random")
	}
	if !VerifyPassword(second, "correct horse battery staple") {
		t.Error("the second hash does not verify")
	}
	if _, err := HashPassword(""); err == nil {
		t.Error("hashing an empty password should be refused")
	}
}

func TestParseDurationAcceptsDays(t *testing.T) {
	cases := map[string]time.Duration{
		"30s": 30 * time.Second,
		"12h": 12 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"90d": 90 * 24 * time.Hour,
		"":    0,
	}
	for in, want := range cases {
		got, err := ParseDuration(in)
		if err != nil {
			t.Errorf("ParseDuration(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseDuration(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseDuration("banana"); err == nil {
		t.Error("an unparseable duration should be an error")
	}
}

func TestValidateRefusesAnOpenPanel(t *testing.T) {
	// A panel with no password would publish the shape of the whole
	// machine to anyone who can reach the port.
	cfg := Default()
	cfg.Password = ""
	if err := cfg.Validate(); err == nil {
		t.Error("a configuration without a password should be refused")
	}
	cfg.Password = "something"
	if err := cfg.Validate(); err != nil {
		t.Errorf("a configuration with a password was refused: %v", err)
	}
	// Demo mode holds no real data and is exempt.
	cfg = Default()
	cfg.Password = ""
	cfg.Demo = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("demo mode should not require a password: %v", err)
	}
}

func TestBasePathNormalisation(t *testing.T) {
	cfg := Default()
	cfg.Password = "x"
	cfg.BasePath = "/"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.BasePath != "" {
		t.Errorf("a lone slash should collapse to no prefix, got %q", cfg.BasePath)
	}
	cfg.BasePath = "lotus"
	if err := cfg.Validate(); err == nil {
		t.Error("a base path without a leading slash should be refused")
	}
}
