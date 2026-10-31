// Parsing tool for channel field tables and configuration values.
//
// The detachment from the page is because it is **data** rather than a view: it describes which
// fields each channel has, which controls to use for each, and the two-way conversion of form text
// to configuration values (JSON).
export const KIND_LABEL: Record<string, string> = {
  dingtalk: "钉钉",
  feishu: "飞书",
  wecom: "企业微信",
  webhook: "通用 Webhook",
  telegram: "Telegram",
  email: "邮件",
};

// Defines the configuration fields for each channel.
//
// The only coupling point is secret_keys — which fields should be rendered as password boxes is
// given by the backend, because only the channel implementation knows which values are credentials
// (the entire Webhook in WeCom is a credential, and DingTalk is just one of the secrets).
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "Webhook 地址",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "加签密钥",
      kind: "password",
      help: "机器人安全设置选「加签」时填写；选「自定义关键词」或未开启安全设置则留空",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "Webhook 地址",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "签名校验密钥", kind: "password", help: "机器人开启「签名校验」时填写，否则留空" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "Webhook 地址",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    { key: "url", label: "目标 URL", kind: "text", placeholder: "https://your-endpoint.example.com/hook" },
    {
      key: "method",
      label: "请求方法",
      kind: "select",
      options: [
        { value: "POST", label: "POST（带请求体）" },
        { value: "PUT", label: "PUT（带请求体）" },
        { value: "PATCH", label: "PATCH（带请求体）" },
        { value: "GET", label: "GET（不带请求体）" },
      ],
    },
    { key: "headers", label: "自定义请求头", kind: "kv", help: "每行 KEY=VALUE，例如 Authorization=Bearer xxx" },
    {
      key: "body_template",
      label: "请求体模板",
      kind: "textarea",
      help:
        "留空用内置默认模板。变量：{{.Title}} {{.Batch}} {{.Count}} {{.HomeURL}} {{.SentAt}}，" +
        "以及 range .Items 下的 .Name/.VulnClass/.Severity/.Summary/.Assets/.DetailURL/.StatusLabel。" +
        "插入字符串请用 {{json .Xxx}} 而不是 {{.Xxx}}，否则标题里的引号会破坏 JSON。",
    },
  ],
  telegram: [
    { key: "bot_token", label: "Bot Token", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "Chat ID", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "API 地址",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "留空用官方地址；自建 Bot API 反代时填写",
    },
  ],
  email: [
    { key: "host", label: "SMTP 服务器", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "端口",
      kind: "number",
      placeholder: "587",
      help: "587 走 STARTTLS；465 请把「隐式 TLS」打开",
    },
    { key: "username", label: "账号", kind: "text" },
    { key: "password", label: "密码 / 授权码", kind: "password" },
    { key: "from", label: "发件人", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "收件人", kind: "list", help: "多个地址用逗号分隔" },
    { key: "tls", label: "隐式 TLS", kind: "switch", help: "465 端口打开；587 保持关闭（会自动 STARTTLS）" },
  ],
};

export const SEVERITY_OPTIONS = [
  { value: "", label: "不限" },
  { value: "low", label: "低危及以上" },
  { value: "medium", label: "中危及以上" },
  { value: "high", label: "高危及以上" },
  { value: "critical", label: "仅严重" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV Parses a text domain with "per line KEY=VALUE".
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs Parses a comma/space-separated list of ids.
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords Parses a list of keywords separated by lines/commas (vulnerability type names may
// contain spaces, so they are separated by lines or commas).
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
