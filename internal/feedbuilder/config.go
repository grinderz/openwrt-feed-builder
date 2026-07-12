package feedbuilder

// Load and validate the YAML configuration.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Source is one raw source entry from the config (kept untyped so each resolver
// can read the fields it cares about, mirroring the Python dict-based design).
type Source map[string]any

func (s Source) str(key string) (string, bool) {
	v, ok := s[key]
	if !ok || v == nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case fmt.Stringer:
		return t.String(), true
	default:
		return fmt.Sprintf("%v", t), true
	}
}

func (s Source) strOr(key, def string) string {
	if v, ok := s.str(key); ok {
		return v
	}
	return def
}

// yamlBool interprets a decoded YAML value as a boolean. yaml.v3 only decodes
// true/false as bool and leaves YAML 1.1 spellings (yes/no/on/off) as strings,
// so those are accepted here too — silently ignoring `enabled: no` would leave
// a source the user disabled still enabled.
func yamlBool(v any) (value, ok bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(t) {
		case "true", "yes", "on", "y":
			return true, true
		case "false", "no", "off", "n":
			return false, true
		}
	}
	return false, false
}

// boolOr returns the value of key as bool, or def when absent. A literal false
// disables; anything missing falls back to def.
func (s Source) boolOr(key string, def bool) bool {
	v, ok := s[key]
	if !ok || v == nil {
		return def
	}
	if b, ok := yamlBool(v); ok {
		return b
	}
	return def
}

func (s Source) strSlice(key string) []string {
	v, ok := s[key]
	if !ok || v == nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if str, ok := item.(string); ok {
			out = append(out, str)
		} else {
			out = append(out, fmt.Sprintf("%v", item))
		}
	}
	return out
}

// SignConfig holds usign signing settings.
type SignConfig struct {
	Enabled      bool
	SecretKey    string // resolved path or ""
	SecretKeyCmd string // shell command that prints the secret key, or ""
	PublicKey    string // resolved path or ""
}

// ServeConfig holds the local HTTP server settings.
type ServeConfig struct {
	Host string
	Port int
}

// Config is the fully resolved configuration.
type Config struct {
	BaseDir       string // directory of the config file; relative paths resolve against it
	OutputDir     string
	CacheDir      string
	Sources       []Source
	Architectures []string
	Targets       []string
	Layout        Layout
	Sign          SignConfig
	Serve         ServeConfig
	BaseURL       string
	FeedPrefix    string // opkg feed-name prefix, e.g. "custom" -> custom_<arch>
}

func asString(v any, def string) string {
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func asBool(v any, def bool) bool {
	if b, ok := yamlBool(v); ok {
		return b
	}
	return def
}

func asInt(v any, def int) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	default:
		return def
	}
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// asStringSlice accepts either a single scalar or a YAML list of scalars.
func asStringSlice(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s := asString(item, ""); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		if s := asString(v, ""); s != "" {
			return []string{s}
		}
		return nil
	}
}

// releaseRE matches a 3-part point release, e.g. "24.10.7".
var releaseRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// loadConfig reads, resolves and validates config.yaml.
func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := yaml.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data == nil {
		data = map[string]any{}
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	base := filepath.Dir(abs)
	resolve := func(p string) string {
		if p == "" {
			return ""
		}
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}

	rawSources, _ := data["sources"].([]any)
	if len(rawSources) == 0 {
		return nil, fmt.Errorf("config has no 'sources'")
	}
	sources := make([]Source, 0, len(rawSources))
	for i, item := range rawSources {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("source #%d is not a mapping", i)
		}
		if _, has := m["type"]; !has {
			return nil, fmt.Errorf("source #%d is missing 'type'", i)
		}
		sources = append(sources, Source(m))
	}

	signRaw := asMap(data["sign"])
	sign := SignConfig{
		Enabled:      asBool(signRaw["enabled"], false),
		SecretKey:    resolve(asString(signRaw["secret_key"], "")),
		SecretKeyCmd: asString(signRaw["secret_key_cmd"], ""),
		PublicKey:    resolve(asString(signRaw["public_key"], "")),
	}
	if sign.Enabled && sign.SecretKey == "" && sign.SecretKeyCmd == "" {
		return nil, fmt.Errorf("sign.enabled is true but neither sign.secret_key " +
			"nor sign.secret_key_cmd is set")
	}
	if sign.SecretKey != "" && sign.SecretKeyCmd != "" {
		return nil, fmt.Errorf("set only one of sign.secret_key or sign.secret_key_cmd")
	}

	serveRaw := asMap(data["serve"])
	serve := ServeConfig{
		Host: asString(serveRaw["host"], "0.0.0.0"),
		Port: asInt(serveRaw["port"], 8080),
	}

	layoutRaw := asMap(data["layout"])
	layout := Layout{
		Versions:    asStringSlice(layoutRaw["version"]),
		DefaultFeed: asString(layoutRaw["default_feed"], "packages"),
		KmodVersion: asString(layoutRaw["kmod_version"], ""),
	}
	if style := asString(layoutRaw["style"], ""); style != "" && style != "official" {
		return nil, fmt.Errorf("layout.style %q is no longer supported; the layout always "+
			"mirrors downloads.openwrt.org/releases/ (drop the 'style' key)", style)
	}
	if len(layout.Versions) == 0 {
		return nil, fmt.Errorf("layout.version is required — one or more point releases, " +
			"e.g. \"24.10.7\" or [\"24.10.6\", \"24.10.7\"]")
	}
	for _, v := range layout.Versions {
		if !releaseRE.MatchString(v) {
			return nil, fmt.Errorf("layout.version entries must be a 3-part point release "+
				"like \"24.10.7\", got %q", v)
		}
		branch := v[:strings.LastIndex(v, ".")]
		if layout.Branch == "" {
			layout.Branch = branch
		} else if layout.Branch != branch {
			return nil, fmt.Errorf("layout.version entries must share one branch: "+
				"%q vs %q.x", v, layout.Branch)
		}
	}

	var architectures []string
	if list, ok := data["architectures"].([]any); ok {
		for _, item := range list {
			architectures = append(architectures, asString(item, ""))
		}
	}

	var targets []string
	if list, ok := data["targets"].([]any); ok {
		for _, item := range list {
			if t := strings.Trim(asString(item, ""), "/"); t != "" {
				targets = append(targets, t)
			}
		}
	}

	return &Config{
		BaseDir:       base,
		OutputDir:     resolve(asString(data["output_dir"], "./releases")),
		CacheDir:      resolve(asString(data["cache_dir"], "./.cache")),
		Sources:       sources,
		Architectures: architectures,
		Targets:       targets,
		Layout:        layout,
		Sign:          sign,
		Serve:         serve,
		BaseURL:       strings.TrimRight(asString(data["base_url"], ""), "/"),
		FeedPrefix:    feedPrefixRE.ReplaceAllString(asString(data["feed_prefix"], "custom"), "_"),
	}, nil
}

// feedPrefixRE sanitizes the configured feed prefix to the characters opkg
// accepts in a feed name (it becomes part of custom_<arch> style labels).
var feedPrefixRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)
