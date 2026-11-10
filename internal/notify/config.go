package notify

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the configuration file of icinga-kubernetes-notify.
type Config struct {
	Database      DatabaseConfig      `yaml:"database"`
	Notifications NotificationsConfig `yaml:"notifications"`
	Watch         WatchConfig         `yaml:"watch"`
	// Listen is the address of the health and metrics endpoint. Empty disables it.
	Listen string `yaml:"listen"`
}

// DatabaseConfig points at the Icinga for Kubernetes database. Read access is enough.
type DatabaseConfig struct {
	Host         string `yaml:"host"`
	Port         int    `yaml:"port"`
	Database     string `yaml:"database"`
	User         string `yaml:"user"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
	TLS          bool   `yaml:"tls"`
}

// NotificationsConfig is the connection to the Icinga Notifications daemon.
type NotificationsConfig struct {
	URL          string `yaml:"url"`
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
	// CAFile is an optional PEM bundle used to verify an https URL.
	CAFile string `yaml:"ca_file"`
	// IcingaWebURL is the base URL of Icinga Web, used for links in notifications.
	IcingaWebURL string        `yaml:"icingaweb_url"`
	Timeout      time.Duration `yaml:"timeout"`
}

// WatchConfig selects what is reported and when.
type WatchConfig struct {
	Interval time.Duration `yaml:"interval"`
	// Kinds to watch: deployment, statefulset, daemonset, replicaset, node, job, cronjob, pod.
	Kinds []string `yaml:"kinds"`
	// Severity maps an Icinga for Kubernetes state to a notification severity
	// (warning, error, critical). States not listed do not raise a problem.
	Severity map[string]string `yaml:"severity"`
	// Clusters limits reporting to these cluster names. Empty means all.
	Clusters []string `yaml:"clusters"`
	// Namespaces and ExcludeNamespaces are glob patterns (path.Match syntax).
	Namespaces        []string      `yaml:"namespaces"`
	ExcludeNamespaces []string      `yaml:"exclude_namespaces"`
	For               time.Duration `yaml:"for"`
	RecoverAfter      time.Duration `yaml:"recover_after"`
	GoneAfter         time.Duration `yaml:"gone_after"`
	DaemonStaleAfter  time.Duration `yaml:"daemon_stale_after"`
}

// kindInfo describes where a kind lives in the database and in Icinga for Kubernetes Web.
type kindInfo struct {
	table      string
	webPath    string
	namespaced bool
}

var kinds = map[string]kindInfo{
	"deployment":  {"deployment", "deployment", true},
	"statefulset": {"stateful_set", "statefulset", true},
	"daemonset":   {"daemon_set", "daemonset", true},
	"replicaset":  {"replica_set", "replicaset", true},
	"job":         {"job", "job", true},
	"cronjob":     {"cron_job", "cronjob", true},
	"pod":         {"pod", "pod", true},
	"node":        {"node", "node", false},
}

// DefaultConfig returns the defaults that a config file is merged onto.
func DefaultConfig() Config {
	return Config{
		Database:      DatabaseConfig{Port: 3306, Database: "kubernetes"},
		Notifications: NotificationsConfig{Timeout: 10 * time.Second},
		Listen:        ":8080",
		Watch: WatchConfig{
			Interval:         15 * time.Second,
			Kinds:            []string{"deployment", "statefulset", "daemonset", "node", "job", "cronjob"},
			Severity:         map[string]string{"critical": SevCritical, "unknown": SevError},
			For:              2 * time.Minute,
			RecoverAfter:     time.Minute,
			GoneAfter:        5 * time.Minute,
			DaemonStaleAfter: 2 * time.Minute,
		},
	}
}

// LoadConfig reads the YAML file at path (optional if empty) and applies environment
// overrides for secrets and endpoints.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		// Lists and maps from the file replace the defaults instead of merging into them,
		// otherwise a default such as "unknown: error" could never be removed.
		def := cfg.Watch
		cfg.Watch.Kinds, cfg.Watch.Severity = nil, nil
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true) // a typo must not silently fall back to a default
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
		if cfg.Watch.Kinds == nil {
			cfg.Watch.Kinds = def.Kinds
		}
		if cfg.Watch.Severity == nil {
			cfg.Watch.Severity = def.Severity
		}
	}
	env := func(name string, dst *string) {
		if v, ok := os.LookupEnv("IKN_" + name); ok {
			*dst = v
		}
	}
	env("DATABASE_HOST", &cfg.Database.Host)
	env("DATABASE_USER", &cfg.Database.User)
	env("DATABASE_PASSWORD", &cfg.Database.Password)
	env("DATABASE_PASSWORD_FILE", &cfg.Database.PasswordFile)
	env("NOTIFICATIONS_URL", &cfg.Notifications.URL)
	env("NOTIFICATIONS_USERNAME", &cfg.Notifications.Username)
	env("NOTIFICATIONS_PASSWORD", &cfg.Notifications.Password)
	env("NOTIFICATIONS_PASSWORD_FILE", &cfg.Notifications.PasswordFile)
	env("NOTIFICATIONS_ICINGAWEB_URL", &cfg.Notifications.IcingaWebURL)

	var err error
	if cfg.Database.Password, err = secret(cfg.Database.Password, cfg.Database.PasswordFile, "database"); err != nil {
		return cfg, err
	}
	if cfg.Notifications.Password, err = secret(cfg.Notifications.Password, cfg.Notifications.PasswordFile, "notifications"); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

func secret(value, file, section string) (string, error) {
	if file == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("%s: password and password_file are mutually exclusive", section)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%s: read password_file: %w", section, err)
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

// Validate reports every problem at once so a broken config is fixed in one pass.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Database.Host == "" {
		add("database.host is required")
	}
	if c.Database.User == "" {
		add("database.user is required")
	}
	if c.Notifications.URL == "" {
		add("notifications.url is required")
	} else if u, err := url.Parse(c.Notifications.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("notifications.url must be an http(s) URL")
	}
	if c.Notifications.Username == "" || c.Notifications.Password == "" {
		add("notifications.username and notifications.password are required")
	}
	if c.Notifications.IcingaWebURL != "" {
		if u, err := url.Parse(c.Notifications.IcingaWebURL); err != nil || !u.IsAbs() {
			add("notifications.icingaweb_url must be an absolute URL")
		}
	}
	if len(c.Watch.Kinds) == 0 {
		add("watch.kinds must not be empty")
	}
	for _, k := range c.Watch.Kinds {
		if _, ok := kinds[k]; !ok {
			add("watch.kinds: unknown kind %q", k)
		}
	}
	for state, sev := range c.Watch.Severity {
		switch state {
		case "critical", "warning", "unknown":
		default:
			add("watch.severity: unknown state %q (use critical, warning, unknown)", state)
		}
		switch sev {
		case SevWarning, SevError, SevCritical:
		default:
			add("watch.severity.%s: unknown severity %q (use warning, error, critical)", state, sev)
		}
	}
	if len(c.Watch.Severity) == 0 {
		add("watch.severity must map at least one state")
	}
	if c.Watch.Interval < time.Second {
		add("watch.interval must be at least 1s")
	}
	if c.Watch.For < 0 || c.Watch.RecoverAfter < 0 || c.Watch.GoneAfter < 0 {
		add("watch.for, watch.recover_after and watch.gone_after must not be negative")
	}
	if c.Watch.DaemonStaleAfter < 30*time.Second {
		add("watch.daemon_stale_after must be at least 30s")
	}
	return errors.Join(errs...)
}
