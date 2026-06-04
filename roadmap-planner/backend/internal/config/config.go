/*
Copyright 2024 The AlaudaDevops Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Config represents the application configuration
type Config struct {
	Debug         bool          `mapstructure:"debug"`
	Logger        Logger        `mapstructure:"logger"`
	Jira          Jira          `mapstructure:"jira"`
	Server        Server        `mapstructure:"server"`
	Cache         Cache         `mapstructure:"cache"`
	Metrics       Metrics       `mapstructure:"metrics"`
	Storage       Storage       `mapstructure:"storage"`
	GitHub        GitHub        `mapstructure:"github"`
	GitLab        GitLab        `mapstructure:"gitlab"`
	TeamAnalytics TeamAnalytics `mapstructure:"team_analytics"`
}

// TeamAnalytics holds the pillar attribution map used by /api/contributions/pillars
// (and the slice explorer's pillar axis).
//
// Layout: pillar name → list of repo globs and Jira component names that
// belong to it. A single repo or component MAY appear under several pillars;
// each appearance credits the corresponding pillar independently when a PR
// or issue lands. Member-level totals stay independent of this mapping —
// pillar attribution is per-PR / per-issue, not per-member.
//
// Repo entries support glob (`*`) wildcards. Owner case-insensitive match.
// Jira component names match exactly (case-insensitive).
//
// Example:
//
//	team_analytics:
//	  pillars:
//	    "CI/CD":
//	      repos: ["alaudadevops/tektoncd-*", "alaudadevops/helm*"]
//	      components: ["Tekton"]
//	    "Tool Deployment":
//	      repos: ["alaudadevops/harbor*", "alaudadevops/helm*"]
type TeamAnalytics struct {
	Pillars map[string]PillarMapping `mapstructure:"pillars" yaml:"pillars"`
	// GitHubLoginPrefills maps a Jira member id to a GitHub login. On
	// every successful Jira sync the backend walks this map and writes
	// each gh login onto the corresponding member iff the member's
	// `github_login` is currently empty — manual edits in the team
	// drawer always win, so once an operator overrides a value here
	// the prefill never clobbers it. Empty map disables the feature.
	//
	// Example:
	//
	//	team_analytics:
	//	  github_login_prefills:
	//	    daniel: danielfbm
	//	    jtcheng: chengjingtao
	GitHubLoginPrefills map[string]string `mapstructure:"github_login_prefills" yaml:"github_login_prefills"`
	// GitLabUsernamePrefills mirrors GitHubLoginPrefills for the GitLab
	// side. Same one-shot-on-empty semantics — manual UI edits always
	// win. Use when the optional email-based auto-prefill (gitlab.users
	// search) doesn't catch a teammate.
	//
	// Example:
	//
	//	team_analytics:
	//	  gitlab_username_prefills:
	//	    daniel: daniel
	//	    jtcheng: chengjingtao
	GitLabUsernamePrefills map[string]string `mapstructure:"gitlab_username_prefills" yaml:"gitlab_username_prefills"`
	// MemberDenylist (W1) — operator-curated list of Jira member ids
	// that should be excluded from the team-analytics rollups and from
	// every contributions API response. Subtracted from the prefill-
	// derived allowlist.
	MemberDenylist []string `mapstructure:"member_denylist" yaml:"member_denylist"`
	// BotLogins (W2) — explicit list of GitHub / GitLab logins that
	// should be reassigned to the synthetic `bot` member instead of
	// landing under their raw login. Match is exact + case-folded; no
	// suffix magic. The synthetic `bot` member is auto-created by the
	// Jira sync.
	BotLogins []string `mapstructure:"bot_logins" yaml:"bot_logins"`
	// Statuses (W4) — configurable status → lane map for the sprint
	// card. Each lane (todo/in_progress/done/cancelled) is optional;
	// empty lanes inherit DefaultStatusLanes() which covers the live
	// DEVOPS Jira workflow (37 statuses, English + Chinese).
	Statuses StatusLanes `mapstructure:"statuses" yaml:"statuses"`
}

// StatusLanes is the configurable four-lane sprint classifier.
type StatusLanes struct {
	Todo       []string `mapstructure:"todo" yaml:"todo"`
	InProgress []string `mapstructure:"in_progress" yaml:"in_progress"`
	Done       []string `mapstructure:"done" yaml:"done"`
	Cancelled  []string `mapstructure:"cancelled" yaml:"cancelled"`
}

// PillarMapping is one bucket inside TeamAnalytics.Pillars.
//
// VersionPrefixes is the fallback path for issues that have no
// `components` set but do have `fixVersions`. A version `name` is
// credited to this pillar if it begins with `<prefix>-` (the
// "<component>-v<X.Y.Z>" convention used by the DEVOPS Jira project)
// or matches the entry as a glob via path.Match.
type PillarMapping struct {
	Repos           []string `mapstructure:"repos" yaml:"repos"`
	Components      []string `mapstructure:"components" yaml:"components"`
	VersionPrefixes []string `mapstructure:"version_prefixes" yaml:"version_prefixes"`
}

// Storage configures the durable team-analytics store.
//
// Type supports "sqlite" (default, file-based, single-binary) or
// "postgres" (DSN in DSN, recommended for shared deployments). Path is
// only consulted for sqlite; DSN is only consulted for postgres.
//
// BackfillDays controls how far back the *first* collection cycle
// reaches. After the first run we resume incrementally from the
// previous run's timestamp. Default 180.
type Storage struct {
	Enabled      bool   `mapstructure:"enabled"`
	Type         string `mapstructure:"type"`          // "sqlite" | "postgres"
	Path         string `mapstructure:"path"`          // e.g. "./data/roadmap.db" (sqlite)
	DSN          string `mapstructure:"dsn"`           // e.g. "postgres://…" (postgres)
	BackfillDays int    `mapstructure:"backfill_days"` // first-run window, default 180
}

// GitHub configures the team-analytics GitHub ingestion.
//
// Repos are listed as "owner/name" strings; "owner/name:component" syntax
// also works to attach a component label to every PR fetched from that
// repo.
//
// Two auth modes (mutually exclusive — App wins when both are set):
//
//   - PAT path:  set Token, or the GITHUB_TOKEN env var.
//   - App path:  set App.{AppID, InstallationID, PrivateKey*}.
//
// App auth is recommended for production: per-installation rate-limit
// pool that scales with repo count, no human-account dependency, and a
// proper audit trail in the org log.
//
// BackfillDays overrides Storage.BackfillDays for the GitHub side
// specifically. Useful when GitHub history is shorter than Jira history
// (e.g., the repo was migrated recently).
type GitHub struct {
	Enabled      bool      `mapstructure:"enabled"`
	BaseURL      string    `mapstructure:"base_url"` // empty -> api.github.com
	Token        string    `mapstructure:"token"`
	App          GitHubApp `mapstructure:"app"`
	SyncInterval string    `mapstructure:"sync_interval"` // duration, e.g. "30m"
	Repos        []string  `mapstructure:"repos"`
	ProjectKey   string    `mapstructure:"project_key"`   // for the default Linker (defaults to Jira.Project)
	BackfillDays int       `mapstructure:"backfill_days"` // first-run window override; 0 = inherit Storage.BackfillDays
}

// GitHubApp configures the App-installation auth path.
//
// PrivateKeyPEM holds the raw PEM (multiline). PrivateKeyPath points to
// a file on disk; mount the App's downloaded .pem from a Kubernetes
// Secret as a file and set this. PrivateKeyPEM wins if both are set.
type GitHubApp struct {
	AppID          int64  `mapstructure:"app_id"`
	InstallationID int64  `mapstructure:"installation_id"`
	PrivateKeyPath string `mapstructure:"private_key_path"`
	PrivateKeyPEM  string `mapstructure:"private_key_pem"`
}

// Configured reports whether enough App fields are set to mint a token.
func (g GitHubApp) Configured() bool {
	return g.AppID > 0 && g.InstallationID > 0 &&
		(g.PrivateKeyPath != "" || g.PrivateKeyPEM != "")
}

// GitLab configures the team-analytics GitLab ingestion.
//
// Auth is a personal access token with read_api scope over the projects
// to track. Bind GITLAB_TOKEN if the deployment doesn't want the secret
// in the ConfigMap. BaseURL defaults to gitlab.com — set it to the
// internal instance (https://gitlab-ce.alauda.cn) for the alauda fleet.
//
// Groups are listed as glob specs (see internal/gitlab.ParseGroupSpec):
//   - "group/sub/proj" — exact project lookup
//   - "group/*"        — direct children of `group`
//   - "group/**"       — entire subtree, recursively
//
// HydrateDiff turns on per-merged-MR additions/deletions/changes_count
// fetches. Off by default because the data isn't surfaced on the
// dashboard yet and it ~doubles the API call budget per cycle.
type GitLab struct {
	Enabled         bool     `mapstructure:"enabled"`
	BaseURL         string   `mapstructure:"base_url"`
	Token           string   `mapstructure:"token"`
	SyncInterval    string   `mapstructure:"sync_interval"`
	Groups          []string `mapstructure:"groups"`
	ProjectKey      string   `mapstructure:"project_key"`
	BackfillDays    int      `mapstructure:"backfill_days"`
	HydrateDiff     bool     `mapstructure:"hydrate_diff"`
	IncludeArchived bool     `mapstructure:"include_archived"`
	// MemberInstanceSweep turns on the W1 Pass B: after the group-scoped
	// fetch finishes, sweep `/merge_requests?scope=all&author_username=<u>`
	// for every allowlisted member whose GitLab username we know. Off
	// by default so existing installs upgrade with no behaviour change;
	// flip on once `team_analytics.gitlab_username_prefills` is curated.
	MemberInstanceSweep bool `mapstructure:"member_instance_sweep"`
}

// Logger represents logger configuration settings
type Logger struct {
	Level       string `mapstructure:"level"`
	Development bool   `mapstructure:"development"`
	Encoding    string `mapstructure:"encoding"`
}

// Jira represents Jira configuration settings
//
// Sync* fields are consumed by the team-analytics jirasync package; they
// are ignored when storage.enabled is false.
type Jira struct {
	BaseURL          string   `mapstructure:"base_url"`
	Username         string   `mapstructure:"username"`
	Password         string   `mapstructure:"password"`
	Project          string   `mapstructure:"project"`
	Quarters         []string `mapstructure:"quarters"`
	SyncEnabled      bool     `mapstructure:"sync_enabled"`
	SyncInterval     string   `mapstructure:"sync_interval"`      // e.g. "30m"
	BackfillDays     int      `mapstructure:"backfill_days"`      // first-run window; 0 = inherit storage.backfill_days
	StoryPointsField string   `mapstructure:"story_points_field"` // optional Jira customfield id
	SprintField      string   `mapstructure:"sprint_field"`       // optional Jira customfield id
}

// Server represents server configuration settings
type Server struct {
	Port            int    `mapstructure:"port"`
	CORS            CORS   `mapstructure:"cors"`
	StaticFilesPath string `mapstructure:"static_files_path"`
}

// CORS represents CORS configuration settings
type CORS struct {
	AllowedOrigins []string `mapstructure:"allowed_origins"`
}

// Cache represents cache configuration settings
type Cache struct {
	TTL             string `mapstructure:"ttl"`
	RefreshInterval string `mapstructure:"refresh_interval"`
}

// Metrics represents metrics system configuration
type Metrics struct {
	Enabled            bool             `mapstructure:"enabled"`
	CollectionInterval string           `mapstructure:"collection_interval"`
	HistoricalDays     int              `mapstructure:"historical_days"`
	Prometheus         PrometheusConfig `mapstructure:"prometheus"`
	Filters            []OptionsConfig  `mapstructure:"filters"`
	Calculators        []OptionsConfig  `mapstructure:"calculators"`
	// ExcludePlugins lists component names dropped from all component
	// dimensions (releases and issue components) during collection.
	// Implements plan D6 — v3-era plugins (katanomi, knative, jenkins,
	// tekton-operator) must not dilute v4 team metrics. Exact match.
	ExcludePlugins []string `mapstructure:"exclude_plugins"`
}

// IsPluginExcluded reports whether a component name is in the
// exclude_plugins list.
func (c *Metrics) IsPluginExcluded(name string) bool {
	for _, p := range c.ExcludePlugins {
		if p == name {
			return true
		}
	}
	return false
}

// PrometheusConfig represents Prometheus exporter configuration
type PrometheusConfig struct {
	Enabled   bool   `mapstructure:"enabled"`
	Path      string `mapstructure:"path"`
	Namespace string `mapstructure:"namespace"`
}

type OptionsConfig struct {
	Name    string                 `mapstructure:"name"`
	Enabled bool                   `mapstructure:"enabled"`
	Options map[string]interface{} `mapstructure:"options"`
}

// GetOption retrieves an option value with a default fallback
func (b OptionsConfig) GetOption(key string, defaultValue interface{}) interface{} {
	if val, exists := b.Options[key]; exists {
		return val
	}
	return defaultValue
}

// GetIntOption retrieves an int option with a default fallback
func (b OptionsConfig) GetIntOption(key string, defaultValue int) int {
	val := b.GetOption(key, defaultValue)
	switch v := val.(type) {
	case int:
		return v
	case float64:
		return int(v)
	case int64:
		return int(v)
	default:
		return defaultValue
	}
}

// GetFloat64Option retrieves a float64 option with a default fallback
func (b OptionsConfig) GetFloat64Option(key string, defaultValue float64) float64 {
	val := b.GetOption(key, defaultValue)
	switch v := val.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return defaultValue
	}
}

// GetStringOption retrieves a string option with a default fallback
func (b OptionsConfig) GetStringOption(key string, defaultValue string) string {
	val := b.GetOption(key, defaultValue)
	if s, ok := val.(string); ok {
		return s
	}
	return defaultValue
}

// GetStringSliceOption retrieves a string slice option with a default fallback
func (b OptionsConfig) GetStringSliceOption(key string, defaultValue []string) []string {
	val := b.GetOption(key, nil)
	if val == nil {
		return defaultValue
	}
	switch v := val.(type) {
	case []string:
		return v
	case []interface{}:
		result := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	default:
		return defaultValue
	}
}

// // CalculatorConfig represents configuration for a metric calculator
// type CalculatorConfig OptionsConfig

// // FilterConfig represents configuration for a data filter
// // used to filter data using options
// type FilterConfig OptionsConfig

func (c *Metrics) GetFilter(name string) *OptionsConfig {
	for _, filter := range c.Filters {
		if filter.Name == name {
			return &filter
		}
	}
	return nil
}

func (c *Metrics) GetCalculator(name string) *OptionsConfig {
	for _, calculator := range c.Calculators {
		if calculator.Name == name {
			return &calculator
		}
	}
	return nil
}

// Load loads the configuration from environment variables and config files
func Load() (*Config, error) {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	viper.AddConfigPath("./config")
	viper.AddConfigPath("$HOME/.roadmap-planner")

	// Set default values
	viper.SetDefault("debug", false)
	viper.SetDefault("logger.level", "info")
	viper.SetDefault("logger.development", false)
	viper.SetDefault("logger.encoding", "json")
	viper.SetDefault("server.port", 8080)
	viper.SetDefault("server.static_file_path", "../frontend/build")
	viper.SetDefault("server.cors.allowed_origins", []string{"http://localhost:3000"})
	viper.SetDefault("jira.project", "DEVOPS")
	viper.SetDefault("jira.quarters", []string{"2025Q1", "2025Q2", "2025Q3", "2025Q4", "2026Q1", "2026Q2", "2026Q4"})
	viper.SetDefault("jira.sync_enabled", false)
	viper.SetDefault("jira.sync_interval", "30m")
	viper.SetDefault("jira.backfill_days", 0)
	viper.SetDefault("jira.story_points_field", "")
	viper.SetDefault("jira.sprint_field", "")
	viper.SetDefault("cache.ttl", "5m")
	viper.SetDefault("cache.refresh_interval", "1m")

	// Metrics defaults
	viper.SetDefault("metrics.enabled", false)
	viper.SetDefault("metrics.collection_interval", "5m")
	viper.SetDefault("metrics.historical_days", 365)
	viper.SetDefault("metrics.prometheus.enabled", true)
	viper.SetDefault("metrics.prometheus.path", "/metrics")
	viper.SetDefault("metrics.prometheus.namespace", "roadmap")
	viper.SetDefault("metrics.filters", []OptionsConfig{})

	// Storage defaults
	viper.SetDefault("storage.enabled", false)
	viper.SetDefault("storage.type", "sqlite")
	viper.SetDefault("storage.path", "./data/roadmap.db")
	viper.SetDefault("storage.dsn", "")
	viper.SetDefault("storage.backfill_days", 365)

	// GitLab defaults
	viper.SetDefault("gitlab.enabled", false)
	viper.SetDefault("gitlab.base_url", "")
	viper.SetDefault("gitlab.sync_interval", "30m")
	viper.SetDefault("gitlab.groups", []string{})
	viper.SetDefault("gitlab.backfill_days", 0)
	viper.SetDefault("gitlab.hydrate_diff", true)
	viper.SetDefault("gitlab.include_archived", false)
	viper.SetDefault("gitlab.member_instance_sweep", false)

	// GitHub defaults
	viper.SetDefault("github.enabled", false)
	viper.SetDefault("github.base_url", "")
	viper.SetDefault("github.sync_interval", "30m")
	viper.SetDefault("github.repos", []string{})
	viper.SetDefault("github.backfill_days", 0) // inherit storage.backfill_days
	viper.SetDefault("github.app.app_id", 0)
	viper.SetDefault("github.app.installation_id", 0)
	viper.SetDefault("github.app.private_key_path", "")

	// Environment variable mapping
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	// Bind environment variables
	_ = viper.BindEnv("jira.base_url", "JIRA_BASE_URL")
	_ = viper.BindEnv("jira.username", "JIRA_USERNAME")
	_ = viper.BindEnv("jira.password", "JIRA_PASSWORD")
	_ = viper.BindEnv("jira.sync_enabled", "JIRA_SYNC_ENABLED")
	_ = viper.BindEnv("jira.sync_interval", "JIRA_SYNC_INTERVAL")
	_ = viper.BindEnv("jira.backfill_days", "JIRA_BACKFILL_DAYS")
	_ = viper.BindEnv("jira.story_points_field", "JIRA_STORY_POINTS_FIELD")
	_ = viper.BindEnv("jira.sprint_field", "JIRA_SPRINT_FIELD")
	_ = viper.BindEnv("server.static_files_path", "STATIC_FILES_PATH")
	_ = viper.BindEnv("server.port", "SERVER_PORT")
	_ = viper.BindEnv("debug", "DEBUG")
	_ = viper.BindEnv("metrics.enabled", "METRICS_ENABLED")
	_ = viper.BindEnv("metrics.collection_interval", "METRICS_COLLECTION_INTERVAL")
	_ = viper.BindEnv("metrics.historical_days", "METRICS_HISTORICAL_DAYS")
	_ = viper.BindEnv("storage.enabled", "STORAGE_ENABLED")
	_ = viper.BindEnv("storage.type", "STORAGE_TYPE")
	_ = viper.BindEnv("storage.path", "STORAGE_PATH")
	_ = viper.BindEnv("storage.dsn", "STORAGE_DSN")
	_ = viper.BindEnv("storage.backfill_days", "STORAGE_BACKFILL_DAYS")
	_ = viper.BindEnv("github.enabled", "GITHUB_ENABLED")
	_ = viper.BindEnv("github.token", "GITHUB_TOKEN")
	_ = viper.BindEnv("github.base_url", "GITHUB_BASE_URL")
	_ = viper.BindEnv("github.sync_interval", "GITHUB_SYNC_INTERVAL")
	_ = viper.BindEnv("github.backfill_days", "GITHUB_BACKFILL_DAYS")
	_ = viper.BindEnv("github.app.app_id", "GITHUB_APP_ID")
	_ = viper.BindEnv("github.app.installation_id", "GITHUB_APP_INSTALLATION_ID")
	_ = viper.BindEnv("github.app.private_key_path", "GITHUB_APP_PRIVATE_KEY_PATH")
	_ = viper.BindEnv("github.app.private_key_pem", "GITHUB_APP_PRIVATE_KEY_PEM")
	_ = viper.BindEnv("gitlab.enabled", "GITLAB_ENABLED")
	_ = viper.BindEnv("gitlab.token", "GITLAB_TOKEN")
	_ = viper.BindEnv("gitlab.base_url", "GITLAB_BASE_URL")
	_ = viper.BindEnv("gitlab.sync_interval", "GITLAB_SYNC_INTERVAL")
	_ = viper.BindEnv("gitlab.backfill_days", "GITLAB_BACKFILL_DAYS")

	// Read config file if it exists
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
	}

	var config Config
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Viper lowercases map keys on unmarshal — that destroys pillar
	// names like "CI/CD" or "DevOps - RFEs & NFR". Re-parse the
	// team_analytics block straight from the config file so the
	// configured names round-trip exactly.
	if path := viper.ConfigFileUsed(); path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			var preserve struct {
				TeamAnalytics TeamAnalytics `yaml:"team_analytics"`
			}
			if uerr := yaml.Unmarshal(raw, &preserve); uerr == nil && len(preserve.TeamAnalytics.Pillars) > 0 {
				// Preserve the YAML-side pillar names (case-sensitive),
				// but merge back the viper-loaded denylist + prefills
				// so they aren't lost when the YAML file omits them.
				if len(preserve.TeamAnalytics.MemberDenylist) == 0 {
					preserve.TeamAnalytics.MemberDenylist = config.TeamAnalytics.MemberDenylist
				}
				if len(preserve.TeamAnalytics.GitHubLoginPrefills) == 0 {
					preserve.TeamAnalytics.GitHubLoginPrefills = config.TeamAnalytics.GitHubLoginPrefills
				}
				if len(preserve.TeamAnalytics.GitLabUsernamePrefills) == 0 {
					preserve.TeamAnalytics.GitLabUsernamePrefills = config.TeamAnalytics.GitLabUsernamePrefills
				}
				config.TeamAnalytics = preserve.TeamAnalytics
			}
		}
	}

	return &config, nil
}
