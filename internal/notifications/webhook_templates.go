package notifications

// WebhookTemplate represents a webhook template for popular services
type WebhookTemplate struct {
	Service         string            `json:"service"`
	Name            string            `json:"name"`
	URLPattern      string            `json:"urlPattern"`
	Method          string            `json:"method"`
	Headers         map[string]string `json:"headers"`
	PayloadTemplate string            `json:"payloadTemplate"`
	Instructions    string            `json:"instructions"`
}

// GetWebhookTemplates returns templates for popular webhook services
func GetWebhookTemplates() []WebhookTemplate {
	return []WebhookTemplate{
		{
			Service:    "discord",
			Name:       "Discord Webhook",
			URLPattern: "https://discord.com/api/webhooks/{webhook_id}/{webhook_token}",
			Method:     "POST",
			Headers:    map[string]string{"Content-Type": "application/json"},
			PayloadTemplate: `{
				"username": "Pulse Monitoring",
				"avatar_url": "https://raw.githubusercontent.com/rcourtman/Pulse/main/frontend-modern/public/logo.svg",
				"embeds": [{
					"title": "Pulse Alert: {{.Level | title}}",
					"description": "{{.Message}}",
					"url": "{{.Instance}}",
					"color": {{if eq .Level "critical"}}15158332{{else if eq .Level "warning"}}15105570{{else}}3447003{{end}},
					"fields": [
						{"name": "Resource", "value": "{{.ResourceName}}", "inline": true},
						{"name": "Node", "value": "{{.Node}}", "inline": true},
						{"name": "Type", "value": "{{.Type | title}}", "inline": true},
						{"name": "Value", "value": "{{printf "%.1f" .Value}}%", "inline": true},
						{"name": "Threshold", "value": "{{printf "%.0f" .Threshold}}%", "inline": true},
						{"name": "Duration", "value": "{{.Duration}}", "inline": true}
					],
					"timestamp": "{{.Timestamp}}",
					"footer": {
						"text": "Pulse Monitoring",
						"icon_url": "https://raw.githubusercontent.com/rcourtman/Pulse/main/frontend-modern/public/logo.svg"
					}
				}]
			}`,
			Instructions: "1. In Discord, go to Server Settings > Integrations > Webhooks\n2. Create a new webhook and copy the URL\n3. Paste the URL here (format: https://discord.com/api/webhooks/...)",
		},
		{
			Service:    "telegram",
			Name:       "Telegram Bot",
			URLPattern: "https://api.telegram.org/bot{bot_token}/sendMessage",
			Method:     "POST",
			Headers:    map[string]string{"Content-Type": "application/json"},
			PayloadTemplate: `{
				"chat_id": "{{.ChatID}}",
				"text": "🚨 *Pulse Alert: {{.Level | title}}*\n\n{{.Message}}\n\n📊 *Details:*\n• Resource: {{.ResourceName}}\n• Node: {{.Node}}\n• Type: {{.Type | title}}\n• Value: {{printf "%.1f" .Value}}%\n• Threshold: {{printf "%.0f" .Threshold}}%\n• Duration: {{.Duration}}\n\n🔗 [View in Pulse]({{.Instance}})",
				"parse_mode": "Markdown",
				"disable_web_page_preview": true
			}`,
			Instructions: "1. Create a bot with @BotFather on Telegram\n2. Get your bot token\n3. Get your chat ID by messaging the bot and visiting: https://api.telegram.org/bot<YOUR_BOT_TOKEN>/getUpdates\n4. URL format: https://api.telegram.org/bot<BOT_TOKEN>/sendMessage?chat_id=<CHAT_ID>\n5. IMPORTANT: You MUST include ?chat_id=YOUR_CHAT_ID in the URL",
		},
		{
			Service:    "slack",
			Name:       "Slack Incoming Webhook",
			URLPattern: "https://hooks.slack.com/services/{webhook_path}",
			Method:     "POST",
			Headers:    map[string]string{"Content-Type": "application/json"},
			PayloadTemplate: `{
				"text": "Pulse Alert: {{.Level | title}} - {{.ResourceName}}",
				"blocks": [
					{
						"type": "header",
						"text": {
							"type": "plain_text",
							"text": "Pulse Alert: {{.Level | title}}",
							"emoji": true
						}
					},
					{
						"type": "section",
						"text": {
							"type": "mrkdwn",
							"text": "{{.Message}}"
						}
					},
					{
						"type": "section",
						"fields": [
							{"type": "mrkdwn", "text": "*Resource:*\n{{.ResourceName}}"},
							{"type": "mrkdwn", "text": "*Node:*\n{{.Node}}"},
							{"type": "mrkdwn", "text": "*Type:*\n{{.Type | title}}"},
							{"type": "mrkdwn", "text": "*Value:*\n{{printf "%.1f" .Value}}%"},
							{"type": "mrkdwn", "text": "*Threshold:*\n{{printf "%.0f" .Threshold}}%"},
							{"type": "mrkdwn", "text": "*Duration:*\n{{.Duration}}"}
						]
					},
					{
						"type": "context",
						"elements": [
							{
								"type": "mrkdwn",
								"text": "View in <{{.Instance}}|Proxmox> | Alert ID: {{.ID}}"
							}
						]
					}
				]
			}`,
			Instructions: "1. In Slack, go to Apps > Incoming Webhooks\n2. Add to Slack and choose a channel\n3. Copy the webhook URL and paste it here (format: https://hooks.slack.com/services/...)",
		},
		{
			Service:    "teams",
			Name:       "Microsoft Teams",
			URLPattern: "https://{tenant}.webhook.office.com/webhookb2/{webhook_path}",
			Method:     "POST",
			Headers:    map[string]string{"Content-Type": "application/json"},
			PayloadTemplate: `{
				"@type": "MessageCard",
				"@context": "http://schema.org/extensions",
				"themeColor": "{{if eq .Level "critical"}}FF0000{{else if eq .Level "warning"}}FFA500{{else}}00FF00{{end}}",
				"summary": "Pulse Alert: {{.Level | title}} - {{.ResourceName}}",
				"sections": [{
					"activityTitle": "Pulse Alert: {{.Level | title}}",
					"activitySubtitle": "{{.Message}}",
					"facts": [
						{"name": "Resource", "value": "{{.ResourceName}}"},
						{"name": "Node", "value": "{{.Node}}"},
						{"name": "Type", "value": "{{.Type | title}}"},
						{"name": "Value", "value": "{{printf "%.1f" .Value}}%"},
						{"name": "Threshold", "value": "{{printf "%.0f" .Threshold}}%"},
						{"name": "Duration", "value": "{{.Duration}}"},
						{"name": "Instance", "value": "{{.Instance}}"}
					],
					"markdown": true
				}],
				"potentialAction": [{
					"@type": "OpenUri",
					"name": "View in Proxmox",
					"targets": [{
						"os": "default",
						"uri": "{{.Instance}}"
					}]
				}]
			}`,
			Instructions: "1. In Teams channel, click ... > Connectors\n2. Configure Incoming Webhook\n3. Copy the URL and paste it here\n\nNote: MessageCard format is supported until December 2025. For new implementations, consider using Adaptive Cards.",
		},
		{
			Service:    "pagerduty",
			Name:       "PagerDuty Events API v2",
			URLPattern: "https://events.pagerduty.com/v2/enqueue",
			Method:     "POST",
			Headers: map[string]string{
				"Content-Type": "application/json",
				"Accept":       "application/vnd.pagerduty+json;version=2",
			},
			PayloadTemplate: `{
				"routing_key": "{{.CustomFields.routing_key}}",
				"event_action": "trigger",
				"dedup_key": "{{.ID}}",
				"payload": {
					"summary": "{{.Message}}",
					"timestamp": "{{.Timestamp}}",
					"severity": "{{if eq .Level "critical"}}critical{{else if eq .Level "warning"}}warning{{else}}info{{end}}",
					"source": "{{.Node}}",
					"component": "{{.ResourceName}}",
					"group": "{{.Type}}",
					"class": "{{.Type}}",
					"custom_details": {
						"alert_id": "{{.ID}}",
						"resource_type": "{{.Type}}",
						"current_value": "{{printf "%.1f" .Value}}%",
						"threshold": "{{printf "%.0f" .Threshold}}%",
						"duration": "{{.Duration}}",
						"instance": "{{.Instance}}"
					}
				},
				"client": "Pulse Monitoring",
				"client_url": "{{.Instance}}",
				"links": [{
					"href": "{{.Instance}}",
					"text": "View in Proxmox"
				}]
			}`,
			Instructions: "1. In PagerDuty, go to Configuration > Services\n2. Add an integration > Events API V2\n3. Copy the Integration Key\n4. Add the key as a custom field named 'routing_key'\n\nNote: PagerDuty recommends using Events API v2 for new integrations.",
		},
		{
			Service:    "teams-adaptive",
			Name:       "Microsoft Teams (Adaptive Card)",
			URLPattern: "https://{tenant}.webhook.office.com/webhookb2/{webhook_path}",
			Method:     "POST",
			Headers:    map[string]string{"Content-Type": "application/json"},
			PayloadTemplate: `{
				"type": "message",
				"attachments": [{
					"contentType": "application/vnd.microsoft.card.adaptive",
					"content": {
						"type": "AdaptiveCard",
						"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
						"version": "1.4",
						"body": [
							{
								"type": "TextBlock",
								"text": "Pulse Alert: {{.Level | title}}",
								"weight": "Bolder",
								"size": "Large",
								"color": "{{if eq .Level "critical"}}Attention{{else if eq .Level "warning"}}Warning{{else}}Good{{end}}"
							},
							{
								"type": "TextBlock",
								"text": "{{.Message}}",
								"wrap": true,
								"spacing": "Small"
							},
							{
								"type": "FactSet",
								"facts": [
									{"title": "Resource", "value": "{{.ResourceName}}"},
									{"title": "Node", "value": "{{.Node}}"},
									{"title": "Type", "value": "{{.Type | title}}"},
									{"title": "Current Value", "value": "{{printf "%.1f" .Value}}%"},
									{"title": "Threshold", "value": "{{printf "%.0f" .Threshold}}%"},
									{"title": "Duration", "value": "{{.Duration}}"},
									{"title": "Alert ID", "value": "{{.ID}}"}
								]
							}
						],
						"actions": [{
							"type": "Action.OpenUrl",
							"title": "View in Proxmox",
							"url": "{{.Instance}}"
						}]
					}
				}]
			}`,
			Instructions: "1. In Teams channel, click ... > Connectors\n2. Configure Incoming Webhook\n3. Copy the URL and paste it here\n\nThis uses the modern Adaptive Card format recommended for new implementations.",
		},
		{
			Service:    "generic",
			Name:       "Generic JSON Webhook",
			URLPattern: "",
			Method:     "POST",
			Headers:    map[string]string{"Content-Type": "application/json"},
			PayloadTemplate: `{
				"alert": {
					"id": "{{.ID}}",
					"level": "{{.Level}}",
					"type": "{{.Type}}",
					"resource_name": "{{.ResourceName}}",
					"node": "{{.Node}}",
					"message": "{{.Message}}",
					"value": {{.Value}},
					"threshold": {{.Threshold}},
					"start_time": "{{.StartTime}}",
					"duration": "{{.Duration}}"
				},
				"timestamp": "{{.Timestamp}}",
				"source": "pulse-monitoring"
			}`,
			Instructions: "Configure with your custom webhook endpoint",
		},
	}
}
