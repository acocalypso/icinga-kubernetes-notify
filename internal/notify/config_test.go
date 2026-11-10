package notify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `
database: {host: db, user: ro, password: x}
notifications: {url: "http://n:5680", username: u, password: p}
`

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Watch.For != 2*time.Minute || cfg.Watch.Interval != 15*time.Second || cfg.Database.Port != 3306 {
		t.Fatalf("defaults not applied: %+v", cfg.Watch)
	}
	if cfg.Watch.Severity["critical"] != SevCritical || cfg.Watch.Severity["unknown"] != SevError {
		t.Fatalf("default severity map: %v", cfg.Watch.Severity)
	}
}

func TestLoadConfigSeverityAndKindsReplaceDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, minimal+`
watch:
  kinds: [pod]
  severity: {critical: critical}
  for: 45s
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Watch.Kinds) != 1 || cfg.Watch.Kinds[0] != "pod" {
		t.Fatalf("kinds = %v", cfg.Watch.Kinds)
	}
	if _, ok := cfg.Watch.Severity["unknown"]; ok || len(cfg.Watch.Severity) != 1 {
		t.Fatalf("severity must replace the default map: %v", cfg.Watch.Severity)
	}
	if cfg.Watch.For != 45*time.Second {
		t.Fatalf("for = %v", cfg.Watch.For)
	}
}

func TestLoadConfigRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"unknown key":      minimal + "watch: {intervall: 5s}\n",
		"unknown kind":     minimal + "watch: {kinds: [hpa]}\n",
		"unknown severity": minimal + "watch: {severity: {critical: fatal}}\n",
		"unknown state":    minimal + "watch: {severity: {pending: warning}}\n",
		"bad url":          "database: {host: db, user: ro}\nnotifications: {url: \"n:5680\", username: u, password: p}\n",
		"no credentials":   "database: {host: db, user: ro}\nnotifications: {url: \"http://n:5680\"}\n",
	}
	for name, body := range cases {
		if _, err := LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadConfigReportsAllProblemsAtOnce(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, "watch: {kinds: [hpa]}\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"database.host", "notifications.url", "unknown kind"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestLoadConfigSecretsFromFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	if err := os.WriteFile(pw, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IKN_DATABASE_PASSWORD", "from-env")
	cfg, err := LoadConfig(writeConfig(t, `
database: {host: db, user: ro}
notifications: {url: "http://n:5680", username: u, password_file: `+filepath.ToSlash(pw)+`}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notifications.Password != "from-file" || cfg.Database.Password != "from-env" {
		t.Fatalf("secrets: db=%q notifications=%q", cfg.Database.Password, cfg.Notifications.Password)
	}

	// Both at once is ambiguous and must be refused.
	_, err = LoadConfig(writeConfig(t, `
database: {host: db, user: ro}
notifications: {url: "http://n:5680", username: u, password: p, password_file: `+filepath.ToSlash(pw)+`}
`))
	if err == nil {
		t.Fatal("password and password_file together must be rejected")
	}
}
