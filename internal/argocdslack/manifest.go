package argocdslack

import "strings"

// BotScopes are the OAuth scopes the app asks for at install.
var BotScopes = []string{"chat:write", "commands", "channels:history", "usergroups:read"}

// Manifest is the Slack app manifest for a deployment at baseURL.
func Manifest(cfg Config, baseURL string) map[string]any {
	base := strings.TrimRight(baseURL, "/")
	name := cfg.Setup.AppName
	return map[string]any{
		"_metadata": map[string]any{"major_version": 2, "minor_version": 1},
		"display_information": map[string]any{
			"name":             name,
			"description":      "Deploys and incidents from Argo CD, one message each.",
			"background_color": "#2b2f36",
		},
		"features": map[string]any{
			"app_home": map[string]any{"home_tab_enabled": true, "messages_tab_enabled": false},
			"bot_user": map[string]any{"display_name": name, "always_online": true},
			"slash_commands": []any{map[string]any{
				"command":       cfg.Setup.Command,
				"url":           base + "/slack/commands",
				"description":   "Mute, unmute and check Argo CD apps",
				"usage_hint":    "mute app@cluster 4h reason | mutes | unmute app@cluster | status [app]",
				"should_escape": false,
			}},
		},
		"oauth_config": map[string]any{
			"redirect_urls": []string{base + "/slack/oauth/callback"},
			"scopes":        map[string]any{"bot": BotScopes},
		},
		"settings": map[string]any{
			"event_subscriptions": map[string]any{
				"request_url": base + "/slack/events",
				"bot_events":  []string{"app_home_opened"},
			},
			"interactivity":          map[string]any{"is_enabled": true, "request_url": base + "/slack/interactivity"},
			"org_deploy_enabled":     false,
			"socket_mode_enabled":    false,
			"token_rotation_enabled": false,
		},
	}
}
