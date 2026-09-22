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

	switch typed := v.(type) {
	case string:
		return typed, true
	case fmt.Stringer:
		return typed.String(), true
	default:
		return fmt.Sprintf("%v", typed), true
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
func yamlBool(v any) (bool, bool) {
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

// SignConfig holds the signing settings: usign keys for opkg feeds (24.10
// and older), an ECDSA P-256 PEM keypair for apk feeds (25.12+).
type SignConfig struct {
	Enabled      bool
	SecretKey    string // resolved path or ""
	SecretKeyCmd string // shell command that prints the secret key, or ""
	PublicKey    string // resolved path or ""

	APKSecretKey    string // resolved path or ""
	APKSecretKeyCmd string // shell command that prints the apk secret key, or ""
	APKPublicKey    string // resolved path or ""
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
	FeedPrefix    string  // opkg feed-name prefix, e.g. "custom" -> custom_<arch>
	APKTool       apkTool // apk-tools v3 binary for 25.12+ branches
}

func asString(val any, def string) string {
	if val == nil {
		return def
	}

	if s, ok := val.(string); ok {
		return s
	}

	return fmt.Sprintf("%v", val)
}

func asBool(v any, def bool) bool {
	if b, ok := yamlBool(v); ok {
		return b
	}

	return def
}

func asInt(v any, def int) int {
	switch typed := v.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
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
func asStringSlice(val any) []string {
	switch t := val.(type) {
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
		if s := asString(val, ""); s != "" {
			return []string{s}
		}

		return nil
	}
}

// releaseRE matches a 3-part point release, e.g. "24.10.7".
var releaseRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// defaultServePort is the `serve` port when the config sets none.
const defaultServePort = 8080

// LoadConfig reads, resolves and validates the YAML config at path.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	var data map[string]any
	if err := yaml.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}

	if data == nil {
		data = map[string]any{}
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}

	base := filepath.Dir(abs)
	resolve := func(path string) string {
		if path == "" {
			return ""
		}

		if filepath.IsAbs(path) {
			return path
		}

		return filepath.Join(base, path)
	}

	sources, err := parseSources(data["sources"])
	if err != nil {
		return nil, err
	}

	sign, err := parseSign(asMap(data["sign"]), resolve)
	if err != nil {
		return nil, err
	}

	serveRaw := asMap(data["serve"])
	serve := ServeConfig{
		Host: asString(serveRaw["host"], "0.0.0.0"),
		Port: asInt(serveRaw["port"], defaultServePort),
	}

	layout, err := parseLayout(asMap(data["layout"]))
	if err != nil {
		return nil, err
	}

	if err := checkSignKeys(sign, layout); err != nil {
		return nil, err
	}

	return &Config{
		BaseDir:       base,
		OutputDir:     resolve(asString(data["output_dir"], "./releases")),
		CacheDir:      resolve(asString(data["cache_dir"], "./.cache")),
		Sources:       sources,
		Architectures: parseArchitectures(data["architectures"]),
		Targets:       parseTargets(data["targets"]),
		Layout:        layout,
		Sign:          sign,
		Serve:         serve,
		BaseURL:       strings.TrimRight(asString(data["base_url"], ""), "/"),
		FeedPrefix:    feedPrefixRE.ReplaceAllString(asString(data["feed_prefix"], "custom"), "_"),
		APKTool:       apkTool(asString(data["apk_tool"], "apk")),
	}, nil
}

// feedPrefixRE sanitizes the configured feed prefix to the characters opkg
// accepts in a feed name (it becomes part of custom_<arch> style labels).
var feedPrefixRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// parseSources validates the `sources:` list: every entry a mapping with a type.
func parseSources(raw any) ([]Source, error) {
	rawSources, _ := raw.([]any)
	if len(rawSources) == 0 {
		return nil, fmt.Errorf("%w has no 'sources'", errConfig)
	}

	sources := make([]Source, 0, len(rawSources))
	for idx, item := range rawSources {
		mapping, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: source #%d is not a mapping", errConfig, idx)
		}

		if _, has := mapping["type"]; !has {
			return nil, fmt.Errorf("%w: source #%d is missing 'type'", errConfig, idx)
		}

		sources = append(sources, Source(mapping))
	}

	return sources, nil
}

// parseSign reads the `sign:` section, resolving key paths against the config dir.
func parseSign(signRaw map[string]any, resolve func(string) string) (SignConfig, error) {
	sign := SignConfig{
		Enabled:         asBool(signRaw["enabled"], false),
		SecretKey:       resolve(asString(signRaw["secret_key"], "")),
		SecretKeyCmd:    asString(signRaw["secret_key_cmd"], ""),
		PublicKey:       resolve(asString(signRaw["public_key"], "")),
		APKSecretKey:    resolve(asString(signRaw["apk_secret_key"], "")),
		APKSecretKeyCmd: asString(signRaw["apk_secret_key_cmd"], ""),
		APKPublicKey:    resolve(asString(signRaw["apk_public_key"], "")),
	}
	if sign.SecretKey != "" && sign.SecretKeyCmd != "" {
		return sign, fmt.Errorf("%w: set only one of sign.secret_key or sign.secret_key_cmd", errConfig)
	}

	if sign.APKSecretKey != "" && sign.APKSecretKeyCmd != "" {
		return sign, fmt.Errorf("%w: set only one of sign.apk_secret_key or sign.apk_secret_key_cmd", errConfig)
	}

	return sign, nil
}

// parseLayout reads and validates the `layout:` section.
func parseLayout(layoutRaw map[string]any) (Layout, error) {
	layout := Layout{
		Versions:    asStringSlice(layoutRaw["version"]),
		DefaultFeed: asString(layoutRaw["default_feed"], "packages"),
		KmodVersion: asString(layoutRaw["kmod_version"], ""),
	}
	if style := asString(layoutRaw["style"], ""); style != "" && style != "official" {
		return layout, fmt.Errorf("%w: layout.style %q is no longer supported; the layout always "+
			"mirrors downloads.openwrt.org/releases/ (drop the 'style' key)", errConfig, style)
	}

	if len(layout.Versions) == 0 {
		return layout, fmt.Errorf("%w: layout.version is required — one or more point releases, "+
			"e.g. \"24.10.7\" or [\"24.10.6\", \"24.10.7\"]", errConfig)
	}

	for _, v := range layout.Versions {
		if !releaseRE.MatchString(v) {
			return layout, fmt.Errorf("%w: layout.version entries must be a 3-part point release "+
				"like \"24.10.7\", got %q", errConfig, v)
		}
	}

	if layout.KmodVersion != "" && !releaseRE.MatchString(layout.KmodVersion) {
		return layout, fmt.Errorf("%w: layout.kmod_version must be a 3-part point release "+
			"like \"24.10.7\", got %q", errConfig, layout.KmodVersion)
	}

	return layout, nil
}

// checkSignKeys requires a secret key per format the carried branches use
// when signing is enabled.
func checkSignKeys(sign SignConfig, layout Layout) error {
	if !sign.Enabled {
		return nil
	}

	for _, format := range layout.formats() {
		switch {
		case format == formatIPK && sign.SecretKey == "" && sign.SecretKeyCmd == "":
			return fmt.Errorf("%w: sign.enabled is true but neither sign.secret_key "+
				"nor sign.secret_key_cmd is set (usign key for the opkg branches)", errConfig)
		case format == formatAPK && sign.APKSecretKey == "" && sign.APKSecretKeyCmd == "":
			return fmt.Errorf("%w: sign.enabled is true but neither sign.apk_secret_key "+
				"nor sign.apk_secret_key_cmd is set (EC key for the apk branches "+
				"25.12+; create one with `genkey --apk`)", errConfig)
		}
	}

	return nil
}

func parseArchitectures(raw any) []string {
	var architectures []string

	if list, ok := raw.([]any); ok {
		for _, item := range list {
			architectures = append(architectures, asString(item, ""))
		}
	}

	return architectures
}

// parseTargets reads the `targets:` filter, trimming surrounding slashes.
func parseTargets(raw any) []string {
	var targets []string

	if list, ok := raw.([]any); ok {
		for _, item := range list {
			if t := strings.Trim(asString(item, ""), "/"); t != "" {
				targets = append(targets, t)
			}
		}
	}

	return targets
}
