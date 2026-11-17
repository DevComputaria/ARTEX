export const mentionKinds = [
  { kind: "finding", label: "Vulnerabilidade", alias: "finding" },
  { kind: "asset", label: "Ativo", alias: "asset" },
  { kind: "company", label: "Empresa", alias: "company" },
  { kind: "endpoint", label: "Endpoint", alias: "api" },
  { kind: "ip", label: "IP", alias: "ip" },
  { kind: "app", label: "Aplicação", alias: "app" },
  { kind: "root_domain", label: "Domínio", alias: "domain" },
  { kind: "subdomain", label: "Subdomínio", alias: "subdomain" },
  { kind: "service", label: "Serviço", alias: "service" },
] as const;

export type MentionKind = (typeof mentionKinds)[number]["kind"];
const mentionTokenKinds: Record<MentionKind, string> = {
  finding: "vulnerability",
  asset: "asset",
  company: "company",
  endpoint: "endpoint",
  ip: "IP",
  app: "app",
  root_domain: "domain",
  subdomain: "subdomain",
  service: "service",
};
export interface ChatMention {
  kind: MentionKind;
  id: number;
  label: string;
  description: string;
}

export function activeMention(value: string, caret: number) {
  const before = value.slice(0, caret);
  const start = before.lastIndexOf("@");
  if (start < 0 || (start > 0 && /[\w.+/-]/.test(before[start - 1]))) return null;
  const query = before.slice(start + 1);
  if (/[[\]\r\n@]/.test(query) || query.length > 220) return null;
  return { start, end: caret, query };
}

export function mentionSearch(query: string) {
  const text = query.trimStart().toLowerCase();
  for (const item of mentionKinds) {
    for (const alias of [item.label.toLowerCase(), item.alias]) {
      if (text === alias || text.startsWith(`${alias} `) || (/[^a-z]/.test(alias) && text.startsWith(alias))) {
        return { kind: item.kind, query: query.trimStart().slice(alias.length).trim(), categories: [] };
      }
    }
  }
  const categories = mentionKinds.filter(
    (item) => item.label.toLowerCase().startsWith(text) || item.alias.startsWith(text),
  );
  return { kind: "" as const, query: query.trim(), categories };
}

export function mentionToken(item: ChatMention) {
  const kind = mentionTokenKinds[item.kind] ?? "asset";
  const label = item.label
    .replace(/[[\]]/g, (char) => (char === "[" ? "(" : ")"))
    .replace(/\s+/g, " ")
    .slice(0, 100);
  return `@[${kind}#${item.id} ${label}]`;
}

export function selectedMentions(value: string) {
  return [...value.matchAll(/@\[(vulnerability|asset|company|endpoint|IP|app|domain|subdomain|service)#([0-9]+)(?: ([^\]\r\n]*))?\]/gi)].map(
    (match) => {
      const entry = mentionKinds.find(
        (item) => mentionTokenKinds[item.kind].toLowerCase() === match[1].toLowerCase(),
      );
      return {
        token: match[0],
        label: `${entry?.label ?? match[1]} #${match[2]}${match[3] ? ` · ${match[3]}` : ""}`,
        start: match.index,
      };
    },
  );
}
