// Package config loads Eye of Lotus settings from a JSON file, the
// environment and command line flags, in that order of increasing
// precedence: flags beat the environment, which beats the file.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultPath is where the panel looks for its settings when no path is
// given on the command line.
const DefaultPath = "/etc/eye-of-lotus/config.json"

// Config is the complete runtime configuration.
type Config struct {
	// Listen is the address the HTTP server binds to.
	Listen string `json:"listen"`
	// BasePath is an optional URL prefix, for example "/lotus" when the
	// panel sits behind a reverse proxy on a subpath.
	BasePath string `json:"base_path"`
	// Password is the single password that protects the panel. When it is
	// empty the panel refuses to start unless Demo is set, because an open
	// monitoring endpoint leaks the shape of the whole machine.
	Password string `json:"password"`
	// SessionSecret signs session cookies. It is generated on first run
	// and written back to the config file so that restarts do not log
	// everyone out.
	SessionSecret string `json:"session_secret"`
	// SessionTTL is how long a login lasts.
	SessionTTL Duration `json:"session_ttl"`

	// Database is the SQLite file holding metric history. An empty value
	// disables history entirely, which is useful for a one-off look.
	Database string `json:"database"`
	// SampleInterval is how often the machine is sampled.
	SampleInterval Duration `json:"sample_interval"`
	// StoreInterval is how often a sample is written to the database.
	StoreInterval Duration `json:"store_interval"`
	// RetainRaw, RetainMinute and RetainHour control how long history is
	// kept at each resolution.
	RetainRaw    Duration `json:"retain_raw"`
	RetainMinute Duration `json:"retain_minute"`
	RetainHour   Duration `json:"retain_hour"`

	// Disks limits the reported mount points. Empty means every real
	// filesystem.
	Disks []string `json:"disks"`
	// Ifaces limits the reported network interfaces. Empty means all.
	Ifaces []string `json:"ifaces"`
	// TopProcs is how many processes the snapshot carries.
	TopProcs int `json:"top_procs"`

	// ProbeInterval is how often the internet and DNS probes run.
	ProbeInterval Duration `json:"probe_interval"`
	// ProbeTargets are the TCP endpoints used to decide whether the
	// machine has internet access.
	ProbeTargets []string `json:"probe_targets"`
	// DNSName is the name resolved during the DNS check.
	DNSName string `json:"dns_name"`

	// ProcRoot points the collectors at an alternative root, used by the
	// test suite and by anyone who wants to inspect a mounted image.
	ProcRoot string `json:"proc_root"`

	// Systemd and Docker toggle the optional service collectors.
	Systemd bool `json:"systemd"`
	// SystemdBus is the D-Bus address, empty for the system bus.
	SystemdBus string `json:"systemd_bus"`
	// SystemdDetail fetches per-unit properties, one round trip per unit.
	SystemdDetail bool   `json:"systemd_detail"`
	Docker        bool   `json:"docker"`
	DockerSocket  string `json:"docker_socket"`

	// TrustProxy makes the panel believe X-Forwarded-For, which it should
	// only do when a reverse proxy it controls sits in front.
	TrustProxy bool `json:"trust_proxy"`

	// Demo replaces every collector with a synthetic machine. It is how
	// the screenshots in the README are produced and how the panel can be
	// tried without a server.
	Demo bool `json:"demo"`
	// LogLevel is one of debug, info, warn or error.
	LogLevel string `json:"log_level"`

	// Path records the file the settings came from, for logging.
	Path string `json:"-"`
}

// Duration is a time.Duration that reads and writes itself as a string
// such as "2s" or "7d" in JSON, because a number of nanoseconds in a
// config file is nobody's idea of readable.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalJSON writes the duration in Go's own compact form.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either a Go duration string or a plain number of
// seconds, and additionally understands the day suffix "d".
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*d = 0
		return nil
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		*d = Duration(time.Duration(v * float64(time.Second)))
		return nil
	}
	parsed, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// ParseDuration extends time.ParseDuration with a day suffix, so that a
// retention of "30d" can be written the way an operator thinks about it.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("config: invalid duration %q", s)
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("config: invalid duration %q", s)
	}
	return d, nil
}

// Default returns the settings used when no file exists.
func Default() Config {
	return Config{
		Listen:         "127.0.0.1:9110",
		BasePath:       "",
		SessionTTL:     Duration(12 * time.Hour),
		Database:       "/var/lib/eye-of-lotus/history.db",
		SampleInterval: Duration(2 * time.Second),
		StoreInterval:  Duration(15 * time.Second),
		RetainRaw:      Duration(24 * time.Hour),
		RetainMinute:   Duration(7 * 24 * time.Hour),
		RetainHour:     Duration(90 * 24 * time.Hour),
		TopProcs:       12,
		ProbeInterval:  Duration(15 * time.Second),
		ProbeTargets:   []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"},
		DNSName:        "one.one.one.one",
		Systemd:        true,
		SystemdDetail:  true,
		Docker:         true,
		DockerSocket:   "/var/run/docker.sock",
		LogLevel:       "info",
	}
}

// Load reads the configuration file, applies the environment and then the
// command line. It returns the configuration together with the flags it
// parsed, so that main can print a usage message.
func Load(args []string) (Config, error) {
	cfg := Default()

	fs := flag.NewFlagSet("eye-of-lotus", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		configPath  = fs.String("config", envOr("EOL_CONFIG", DefaultPath), "path to the JSON configuration file")
		listen      = fs.String("listen", "", "address to bind, for example 127.0.0.1:9110")
		basePath    = fs.String("base-path", "", "URL prefix when served behind a reverse proxy")
		database    = fs.String("db", "", "path to the SQLite history file, empty to disable history")
		password    = fs.String("password", "", "panel password; overrides the configuration file")
		demoMode    = fs.Bool("demo", false, "serve a synthetic machine instead of this one")
		showVersion = fs.Bool("version", false, "print the version and exit")
		genHash     = fs.String("hash-password", "", "print a configuration snippet for the given password and exit")
		printConfig = fs.Bool("print-config", false, "print the effective configuration and exit")
		logLevel    = fs.String("log-level", "", "debug, info, warn or error")
		procRoot    = fs.String("proc-root", "", "read /proc and /sys from this root instead of /")
		noHistory   = fs.Bool("no-history", false, "disable metric history even if the configuration enables it")
		healthcheck = fs.Bool("healthcheck", false, "check a running panel over HTTP and exit 0 when it answers")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "eye-of-lotus - a read-only eye on a Linux server\n\nUsage:\n  eye-of-lotus [flags]\n\nFlags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nConfiguration is read from %s unless -config is given.\n", DefaultPath)
	}
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}

	// The two flags that answer a question about the program itself are
	// handled before any file is read. Asking a binary what version it is
	// must not depend on the configuration being present, readable by the
	// caller, or valid: the config is mode 0600 and belongs to the service
	// account, so a plain user asking -version would otherwise be told
	// about a permission problem instead of a version.
	if *showVersion {
		return cfg, ErrShowVersion
	}
	if *genHash != "" {
		hash, err := HashPassword(*genHash)
		if err != nil {
			return cfg, err
		}
		// The hash goes to stdout on its own so that a script can capture
		// it with $(...); the guidance goes to stderr so that it does not
		// end up inside the captured value.
		fmt.Println(hash)
		fmt.Fprintf(os.Stderr, "Paste this hash into the \"password\" key of %s.\n", *configPath)
		return cfg, ErrHandled
	}

	// The file is optional only when it was not named explicitly.
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	if err := loadFile(&cfg, *configPath, explicit); err != nil {
		return cfg, err
	}
	cfg.Path = *configPath

	applyEnv(&cfg)

	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "listen":
			cfg.Listen = *listen
		case "base-path":
			cfg.BasePath = *basePath
		case "db":
			cfg.Database = *database
		case "password":
			cfg.Password = *password
		case "demo":
			cfg.Demo = *demoMode
		case "log-level":
			cfg.LogLevel = *logLevel
		case "proc-root":
			cfg.ProcRoot = *procRoot
		case "no-history":
			if *noHistory {
				cfg.Database = ""
			}
		}
	})

	// The health check needs the listen address, so it stays here rather
	// than beside the other two.
	if *healthcheck {
		return cfg, runHealthcheck(cfg)
	}
	if *printConfig {
		cfg.Password = redact(cfg.Password)
		cfg.SessionSecret = redact(cfg.SessionSecret)
		out, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return cfg, err
		}
		fmt.Println(string(out))
		return cfg, ErrHandled
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	if cfg.SessionSecret == "" {
		secret, err := RandomSecret(32)
		if err != nil {
			return cfg, err
		}
		cfg.SessionSecret = secret
		if err := cfg.saveSecret(*configPath); err != nil {
			// Not fatal: the panel runs, sessions simply end on restart.
			fmt.Fprintf(os.Stderr, "warning: could not persist the session secret to %s: %v\n", *configPath, err)
		}
	}
	return cfg, nil
}

// ErrShowVersion and ErrHandled tell main that the requested work is done.
var (
	ErrShowVersion = errors.New("config: version requested")
	ErrHandled     = errors.New("config: handled by a one-shot flag")
)

func loadFile(cfg *Config, path string, explicit bool) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return nil
		}
		return fmt.Errorf("config: cannot read %s: %w", path, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return fmt.Errorf("config: %s is not valid: %w", path, err)
	}
	return nil
}

// saveSecret rewrites the configuration file with the generated secret so
// that a restart does not invalidate every session.
func (c Config) saveSecret(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		b = []byte("{}")
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	raw["session_secret"] = c.SessionSecret
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// applyEnv lets every setting be overridden as EOL_<NAME>.
func applyEnv(cfg *Config) {
	set := func(key string, target *string) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			*target = v
		}
	}
	set("EOL_LISTEN", &cfg.Listen)
	set("EOL_BASE_PATH", &cfg.BasePath)
	set("EOL_PASSWORD", &cfg.Password)
	set("EOL_DATABASE", &cfg.Database)
	set("EOL_LOG_LEVEL", &cfg.LogLevel)
	set("EOL_DNS_NAME", &cfg.DNSName)
	set("EOL_PROC_ROOT", &cfg.ProcRoot)
	set("EOL_DOCKER_SOCKET", &cfg.DockerSocket)
	set("EOL_SYSTEMD_BUS", &cfg.SystemdBus)
	if v, ok := os.LookupEnv("EOL_TRUST_PROXY"); ok {
		cfg.TrustProxy = v == "1" || strings.EqualFold(v, "true")
	}
	if v, ok := os.LookupEnv("EOL_SYSTEMD"); ok {
		cfg.Systemd = truthy(v)
	}
	if v, ok := os.LookupEnv("EOL_DOCKER"); ok {
		cfg.Docker = truthy(v)
	}
	if v, ok := os.LookupEnv("EOL_DEMO"); ok {
		cfg.Demo = truthy(v)
	}
	// Durations and numbers are read too, because a container deployment
	// normally changes them through the environment rather than a file.
	for key, target := range map[string]*Duration{
		"EOL_SAMPLE_INTERVAL": &cfg.SampleInterval,
		"EOL_STORE_INTERVAL":  &cfg.StoreInterval,
		"EOL_RETAIN_RAW":      &cfg.RetainRaw,
		"EOL_RETAIN_MINUTE":   &cfg.RetainMinute,
		"EOL_RETAIN_HOUR":     &cfg.RetainHour,
		"EOL_SESSION_TTL":     &cfg.SessionTTL,
		"EOL_PROBE_INTERVAL":  &cfg.ProbeInterval,
	} {
		v, ok := os.LookupEnv(key)
		if !ok || v == "" {
			continue
		}
		d, err := ParseDuration(v)
		if err == nil {
			*target = Duration(d)
		}
	}
	if v, ok := os.LookupEnv("EOL_TOP_PROCS"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.TopProcs = n
		}
	}
}

// truthy reads the several spellings of "yes" that appear in the wild.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	default:
		return false
	}
}

// Validate rejects configurations that would start an unsafe or useless
// panel, so that the failure happens at startup with a clear message
// rather than at the first request.
func (c *Config) Validate() error {
	if c.Listen == "" {
		return errors.New("config: listen address must not be empty")
	}
	if c.Demo {
		return nil
	}
	if c.Password == "" {
		return errors.New("config: no password is set. Set one with -password, EOL_PASSWORD or the \"password\" key; a password may be stored as a PBKDF2 hash produced by -hash-password. Use -demo to run with synthetic data and no password")
	}
	if !strings.HasPrefix(c.BasePath, "/") && c.BasePath != "" {
		return errors.New("config: base_path must start with a slash")
	}
	if c.BasePath == "/" {
		c.BasePath = ""
	}
	if c.SampleInterval.D() <= 0 {
		return errors.New("config: sample_interval must be positive")
	}
	if c.TopProcs <= 0 {
		c.TopProcs = 12
	}
	return nil
}

// Addr returns the address to listen on, warning when it is not loopback.
func (c Config) Addr() string { return c.Listen }

// IsPublic reports whether the listen address is reachable from outside
// the machine, which the panel mentions at startup.
func (c Config) IsPublic() bool {
	host := c.Listen
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return false
	}
	return true
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// RandomSecret returns a URL safe random string of n bytes of entropy.
func RandomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	return "<set, hidden>"
}
