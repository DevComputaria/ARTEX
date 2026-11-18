// Centralised status → color/label semantics, reused across the whole app.
// Spec §8.3: Intent / Override / Task / Severity each have a consistent color set.

export type Tone = "neutral" | "blue" | "green" | "amber" | "red" | "rose" | "violet" | "slate";

export const toneClasses: Record<Tone, string> = {
  neutral: "bg-muted text-muted-foreground border-transparent",
  blue: "bg-blue-500/15 text-blue-600 dark:text-blue-400 border-blue-500/20",
  green: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 border-emerald-500/20",
  amber: "bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20",
  red: "bg-red-500/15 text-red-600 dark:text-red-400 border-red-500/20",
  // rose is used for "serious" – a solid high-emphasis, visually clearly higher than the soft red
  // border of "high risk".
  rose: "bg-rose-600 text-white border-rose-600 dark:bg-rose-600 dark:text-white",
  violet: "bg-violet-500/15 text-violet-600 dark:text-violet-400 border-violet-500/20",
  slate: "bg-slate-500/15 text-slate-600 dark:text-slate-400 border-slate-500/20",
};

export const toneDot: Record<Tone, string> = {
  neutral: "bg-muted-foreground",
  blue: "bg-blue-500",
  green: "bg-emerald-500",
  amber: "bg-amber-500",
  red: "bg-red-500",
  rose: "bg-white",
  violet: "bg-violet-500",
  slate: "bg-slate-500",
};

interface StatusMeta {
  label: string;
  tone: Tone;
}

const intent: Record<string, StatusMeta> = {
  open: { label: "Aguardando", tone: "slate" },
  running: { label: "Em execução", tone: "blue" },
  paused: { label: "Pausado", tone: "amber" },
  done: { label: "Concluído", tone: "green" },
  // blocked = Model/API/network failure retry exhausted, intent basically failed (non-target
  // block).
  blocked: { label: "Erro de execução", tone: "red" },
  // exhausted = reached the steps/time budget but was cut off and only writes back a partial result
  // (non-directional exhaustion).
  exhausted: { label: "Orçamento esgotado", tone: "violet" },
  // stopped = History soft-delete status (retained, historical data).
  stopped: { label: "Interrompido", tone: "slate" },
  // deleted = The user faked deletion of the intent (keeping the node and bloodline, see
  // delete_reason for the reason).
  deleted: { label: "Excluído", tone: "slate" },
};

const task: Record<string, StatusMeta> = {
  created: { label: "Criada", tone: "slate" },
  queued: { label: "Na fila", tone: "amber" },
  running: { label: "Em execução", tone: "blue" },
  paused: { label: "Pausada", tone: "amber" },
  done: { label: "Concluída", tone: "green" },
  failed: { label: "Falhou", tone: "red" },
  timeout: { label: "Expirada", tone: "amber" },
};

const severity: Record<string, StatusMeta> = {
  critical: { label: "Crítica", tone: "rose" },
  high: { label: "Alta", tone: "red" },
  medium: { label: "Média", tone: "amber" },
  low: { label: "Baixa", tone: "slate" },
};

const finding: Record<string, StatusMeta> = {
  pending: { label: "Pendente", tone: "amber" },
  in_progress: { label: "Em tratamento", tone: "blue" },
  confirmed: { label: "Confirmada", tone: "red" },
  resolved: { label: "Resolvida", tone: "green" },
  fixed: { label: "Corrigida", tone: "green" },
  false_positive: { label: "Falso positivo", tone: "slate" },
  ignored: { label: "Ignorada", tone: "neutral" },
  duplicate: { label: "Duplicada", tone: "neutral" },
  risk_accepted: { label: "Risco aceito", tone: "violet" },
};

const engine: Record<string, StatusMeta> = {
  exploring: { label: "Explorando", tone: "blue" },
  paused: { label: "Pausado", tone: "amber" },
  stalled: { label: "Paralisado", tone: "red" },
  idle: { label: "Ocioso", tone: "neutral" },
};

const goal: Record<string, StatusMeta> = {
  open: { label: "Em andamento", tone: "blue" },
  met: { label: "Atingido", tone: "green" },
  abandoned: { label: "Abandonado", tone: "slate" },
};

const audit: Record<string, StatusMeta> = {
  allow: { label: "Permitido", tone: "green" },
  block: { label: "Bloqueado", tone: "red" },
};

const node: Record<string, StatusMeta> = {
  observed: { label: "Observado", tone: "slate" },
  confirmed: { label: "Confirmado", tone: "green" },
  tombstoned: { label: "Descartado", tone: "neutral" },
};

// Sending with blue instead of amber: it is not "in trouble", but "has been received, is being
// sent", to distinguish it from the pending semantics of pending.
const delivery: Record<string, StatusMeta> = {
  pending: { label: "Aguardando envio", tone: "amber" },
  sending: { label: "Enviando", tone: "blue" },
  sent: { label: "Entregue", tone: "green" },
  failed: { label: "Falhou", tone: "red" },
  skipped: { label: "Ignorado", tone: "neutral" },
};

const maps = {
  intent,
  task,
  severity,
  finding,
  engine,
  goal,
  audit,
  node,
  delivery,
} as const;

export type StatusDomain = keyof typeof maps;

export function statusMeta(domain: StatusDomain, key: string): StatusMeta {
  return maps[domain][key] ?? { label: key, tone: "neutral" };
}
