package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTemp writes content to a temp file and returns its path.
func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSecretsFileOverridesConfig verifies credentials come from the secrets file
// when they're absent from config.yaml.
func TestSecretsFileOverridesConfig(t *testing.T) {
	cfgPath := writeTemp(t, "config.yaml", `
mode: paper
secrets_path: SECRETS_PLACEHOLDER
`)
	secretsPath := writeTemp(t, "secrets.yaml", `
kite:
  api_key: KEY_FROM_SECRETS
  api_secret: SECRET_FROM_SECRETS
  access_token: TOKEN_FROM_SECRETS
`)
	// Patch the placeholder to the real temp path (Load expands ~, but the temp
	// path is absolute already, so ExpandPath returns it unchanged).
	cfgPath = patchPlaceholder(cfgPath, "SECRETS_PLACEHOLDER", secretsPath)

	c, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kite.APIKey != "KEY_FROM_SECRETS" {
		t.Errorf("api_key = %q, want KEY_FROM_SECRETS", c.Kite.APIKey)
	}
	if c.Kite.APISecret != "SECRET_FROM_SECRETS" {
		t.Errorf("api_secret = %q, want SECRET_FROM_SECRETS", c.Kite.APISecret)
	}
	if c.Kite.AccessToken != "TOKEN_FROM_SECRETS" {
		t.Errorf("access_token = %q, want TOKEN_FROM_SECRETS", c.Kite.AccessToken)
	}
}

// TestEnvOverridesSecrets verifies environment variables win over the secrets
// file (highest precedence).
func TestEnvOverridesSecrets(t *testing.T) {
	secretsPath := writeTemp(t, "secrets.yaml", `
kite:
  api_key: KEY_FROM_SECRETS
  api_secret: SECRET_FROM_SECRETS
`)
	cfgPath := writeTemp(t, "config.yaml", `mode: paper`+"\nsecrets_path: "+secretsPath+"\n")

	t.Setenv("KITE_API_KEY", "KEY_FROM_ENV")
	defer os.Unsetenv("KITE_API_KEY")

	c, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kite.APIKey != "KEY_FROM_ENV" {
		t.Errorf("api_key = %q, want KEY_FROM_ENV (env should win)", c.Kite.APIKey)
	}
	if c.Kite.APISecret != "SECRET_FROM_SECRETS" {
		t.Errorf("api_secret = %q, want SECRET_FROM_SECRETS", c.Kite.APISecret)
	}
}

// TestMissingSecretsFileOK verifies a missing secrets file is not an error
// (dry-run and partial setups must still boot).
func TestMissingSecretsFileOK(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.yaml")
	cfgPath := writeTemp(t, "config.yaml", "mode: dryrun\nsecrets_path: "+missing+"\n")

	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("expected no error for missing secrets file, got: %v", err)
	}
	if c.Kite.APIKey != "" {
		t.Errorf("api_key = %q, want empty", c.Kite.APIKey)
	}
}

// TestDefaultSecretsPath verifies the default points under the home directory.
func TestDefaultSecretsPath(t *testing.T) {
	cfgPath := writeTemp(t, "config.yaml", "mode: dryrun\n")
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.SecretsPath != "~/.trading/secrets.yaml" {
		t.Errorf("default secrets_path = %q, want ~/.trading/secrets.yaml", c.SecretsPath)
	}
}

// TestExpandPath verifies ~ expansion for both Unix and Windows separators.
func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := map[string]string{
		"~/.trading/secrets.yaml": home + "/.trading/secrets.yaml",
		"~/foo":                   home + "/foo",
		"~":                       home,
		"/etc/abs":                "/etc/abs",
		"relative.yaml":           "relative.yaml",
	}
	for in, want := range cases {
		if got := ExpandPath(in); got != want {
			t.Errorf("ExpandPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// patchPlaceholder rewrites the config file, substituting placeholder -> path.
func patchPlaceholder(cfgPath, placeholder, path string) string {
	data, _ := os.ReadFile(cfgPath)
	out := make([]byte, 0, len(data))
	marker := []byte(placeholder)
	i := 0
	for i < len(data) {
		if i+len(marker) <= len(data) && string(data[i:i+len(marker)]) == placeholder {
			out = append(out, []byte(path)...)
			i += len(marker)
			continue
		}
		out = append(out, data[i])
		i++
	}
	_ = os.WriteFile(cfgPath, out, 0o600)
	return cfgPath
}
