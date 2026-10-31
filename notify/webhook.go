package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel is a generic Webhook adapter: user-defined URLs, methods, request headers and JSON
// templates. Its existence eliminates the need to write an implementation for Slack / Mattermost /
// Discord / custom systems — all of which can be covered by one configurable template.
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// There is no official limit on the general webhook, returning 0 means that by default there is no
// stream limit, which can be customized by the user according to his/her capabilities.
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// Masks url and headers: target addresses themselves are often tokenized, and custom headers
// usually contain authorization credentials, both of which appear in the interface return, so they
// are blocked. The cost is that if you want to change one of the headers when editing, you have to
// refill the entire header set (the mask value will be interpreted as "keep the original value") -
// this compromise is deliberate: it is better to fill in more than once, or not to return the
// credentials to the browser.
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// The destination is the url. When changing the url, the headers must be re-stated — otherwise the
// original Authorization header will be sent to the new address as it is, which is the main path
// bypassed by the mask.
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate is the bottom request body when the template is empty: a plain-white JSON
// structure that covers most custom receivers for "get a JSON library".
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData is the context in which the template is exposed to the user.
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt is the time of the current delivery (RFC 3339) for recording by the receiving end.
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel is a readable description of the state change, such as "pending → fixed"; it is
	// empty for non-state changes.
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("缺少目标 URL")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("目标 URL 无效: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("不支持的方法 %s（可用 GET/POST/PUT/PATCH）", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("请求体模板语法错误: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET without request body: inserting content into a query is beyond the scope of the template
	// and does not conform to GET semantics, so GET is only suitable for recipients who are
	// "hit-and-hook".
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// The template renders JSON as a string, which is converted to json.RawMessage as it is
		// sent, avoiding the second rendering of the user's constructed structure into a JSON
		// string.
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("请求体模板渲染结果不是合法 JSON"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// Overrides are allowed, but applied after headers to ensure explicit configuration takes
		// precedence.
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// General Webhook Does not truncate the body (the recipient is the user's own service, the size
	// is determined by body_template), so the whole batch counts as delivered.
	return len(m.Items), nil
}

// renderWebhookBody Renders the request body using a user template (or default template).
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("请求体模板语法错误: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("渲染请求体模板失败: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate Parsing the template.
//
// missingkey=zero makes the missing map key render as zero instead of an error — but this file is
// in a contextual structure, and its main purpose is to prevent.Items from being range when it is
// empty.
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs are auxiliary functions exposed to the template.
var webhookTemplateFuncs = template.FuncMap{
	// json Transforms any string of values into JSON.
	//
	// This function is not a bonus but a necessity: by omitting it, the user can only write
	// {{.Title}} directly into the header, and if the vulnerable header contains quotation marks or
	// a new line, the entire request body is no longer a valid JSON—the recipient will reject it,
	// and the error message pointing to "JSON parsing failed" will not be associated with a
	// quotation mark in the header.
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons is used to embed a JSON fragment into another JSON string value (a layer of string
	// translation).
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// Remove outer quotes: The caller decides whether to add quotation marks.
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
