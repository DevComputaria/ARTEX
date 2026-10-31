"use client";

// Common to LLM retry configurations: Five levels of retry each with "number of times + interval".
//
// Five layers from inside out: build connection (SDK) → empty response (SDK) → same provider
// security window → query meltdown → intent rerun. The first three layers follow the endpoint, so
// each model configuration overrides the global default; the last two layers are process-level and
// only have one global.
//
// All input follows the same set of "empty = unconfigured" semantics, consistent with the
// db.RetryRule backend: Number of times empty/0 = use built-in default | -1 = disable this layer
// retry | >0 = use this number of times interval empty/0 = avoid using the original exponential of
// this layer | >0 = use this fixed millisecond interval

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** Where and by whom this retry occurs */
  where: string;
  /** What kind of errors go to this level—specifically to the status code, don't let anyone guess. */
  trigger: string;
  /** Looks like a bug, but it's not a bug. */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** Default value for placeholders if number is left empty */
  defAttempts: number;
  /** Default policy for spaces left blank, used for placeholders */
  defInterval: string;
  /** The meaning of the number -1 */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "建连重试",
    where: "SDK · 拿到 200 之前",
    trigger:
      "连不上或还没拿到 200：连接重置 / 读写超时 / DNS 失败等网络层错误，以及 HTTP 408、429、500、502、503、504。",
    skips: "其余状态码（400 / 401 / 403 / 404 / 413 / 422 等）都是确定性拒绝，重发也一样失败，直接上抛。",
    desc: "原样重发同一个请求。流一旦开始（已经拿到 200），中途断开就不归这层管了。",
    attemptsLabel: "重试次数",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s 指数（封顶 8s）",
    offHint: "-1 = 一次都不重试，失败立刻上抛",
  },
  empty: {
    title: "空响应重试",
    where: "SDK · 仅 openai 格式",
    trigger:
      "HTTP 200、finish_reason 是正常 stop，但整条响应一个内容块都没有——网关空帧、思考字段丢帧、采样打嗝都会长这样。",
    skips: "因 max_tokens 截断而没有内容的不算（那要靠调高输出上限解决，重发只会再撞一次）。",
    desc: "重发整个 prompt，所以在长上下文上比较贵，次数不宜给大。",
    attemptsLabel: "重试次数",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s 指数（封顶 8s）",
    offHint: "-1 = 空响应直接原样交出",
  },
  stream: {
    title: "同 provider 安全窗口重试",
    where: "本项目 · 未交付输出前",
    trigger:
      "流已经建立（拿到 200）之后才出问题：连接中途断开、供应商 overloaded、流内的 429 / 5xx 错误事件——且一个 token 都还没交给调用方。",
    skips:
      "额度耗尽（402 / insufficient_quota，交给轮询换配置）、上下文过长（413 / context length，交给压缩）、400 / 401 / 403 / 404 / 422 确定性拒绝，都不重试。",
    desc: "在同一个配置上重放同一个请求。因为还没交付任何输出，重放不会重复模型输出或工具执行。",
    attemptsLabel: "重试次数",
    defAttempts: 2,
    defInterval: "0.5s→1s 指数（封顶 4s）",
    offHint: "-1 = 断流直接交给外层的意图重跑",
  },
  breaker: {
    title: "轮询熔断",
    where: "本项目 · 进程级，全局一份",
    trigger:
      "瞬时失败（429、5xx、网络错误）连续累计到阈值时熔断；余额不足（402）、密钥失效（401 / 403）、模型不存在（404）这类确定性失败不看阈值，第一次就熔断。",
    skips: "成功一次即清零，所以偶尔抽风的配置不会被慢慢攒到熔断。",
    desc: "熔断后进入冷却，冷却期内轮询直接跳过这个配置。状态落库，重启不丢。",
    attemptsLabel: "连续失败几次熔断",
    defAttempts: 3,
    defInterval: "1min→5min→30min 梯度",
    offHint: "-1 = 瞬时失败永不熔断（确定性失败仍然熔断）",
  },
  intent: {
    title: "意图重跑",
    where: "本项目 · 进程级，全局一份",
    trigger:
      "前面几层都没兜住：worker 以 model_error 收场——内层重试全部用尽，或者流已经开始交付输出后才断掉（那时重放不安全，只能整条重来）。",
    skips: "额度耗尽已经由轮询换配置处理，不在这里重跑；任务被暂停 / 终止 / 进入收尾时立即让位，不占用退避时间。",
    desc: "整条意图从头再跑一遍。它是最外层，一次重跑意味着里面几层的次数会再乘一遍。",
    attemptsLabel: "重跑次数",
    defAttempts: 2,
    defInterval: "固定 3s",
    offHint: "-1 = 不重跑，该意图直接判为 blocked",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** The milliseconds are used only to reflect next to the input box, so that zeros are not counted. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** Controlled digital input: empty string ↔ 0, intermediate states ("-", "1e") are left locally, not disturbing the parent. */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // The parent follows when a whole set of values is changed (read to policy, switch
  // configuration); it does not go here when you type, because the value is already equal to the
  // result of parsing the local text.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** Two knobs for retry on a layer. idPrefix is used to preserve the htmlFor of label when it appears multiple times on the same page. */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = Compact version in the configuration drawer: omit the expandable description, just leave the phrase "What errors go to this level" */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* What errors go to this layer, specifically to the status code - if you fill the rotary button but you don't see the effect, most of the time the errors are not rooted in this layer. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">触发</span>：{meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">不走这层</span>：{meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`默认 ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            间隔 ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="默认退避"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `固定 ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">留空 = 用默认；{meta.offHint}。</p>}
    </div>
  );
}

/** The model configures the three layers of coverage in the drawer (the three layers that follow the endpoint). */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">重试覆盖</Label>
        <p className="text-muted-foreground text-xs">
          只对这个配置生效，覆盖「重试与退避」里的全局默认。每格留空 = 跟随全局；次数填 -1 = 关掉这层重试；
          间隔填了就用固定间隔取代指数退避。熔断与意图重跑是进程级的，只能在全局那页调。
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** Retry and Avoid tab: Global default value for the five layers. */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`读取重试策略失败：${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // The backend will retrieve the cross-boundary values from the interval and return them,
      // refreshing them directly with the returned values, so that what is seen is what is saved.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("已保存，即时生效（正在跑的这一轮调用仍用旧参数）");
    } catch (e) {
      toast.error(`保存失败：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> 读取重试策略…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        一次模型调用的失败会依次经过五层重试，由内到外：
        <span className="text-foreground"> 建连 → 空响应 → 同 provider 安全窗口 → 轮询熔断 → 意图重跑</span>
        。内层用尽才轮到外层，所以次数是
        <span className="text-foreground">相乘</span>
        的——把每层都拉满，一次抖动能烧掉几十次请求。
        全部留空即当前默认值，与没有这页时的行为完全一致。前三层可以在每个模型配置里单独覆盖。
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          保存
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          全部恢复默认
        </Button>
      </div>
    </div>
  );
}
