"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";

/** Tempo máximo para aguardar a nova versão após a reinicialização. */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // Após preparar a atualização, o processo encerra e o SSE cai; a tela passa a consultar /api/health.
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // quiet também decide se deve ignorar caches de back-end: auto-verifique os caches ao entrar na página (a barra superior foi apenas verificada) e então os post-sites são fechados.
  // O usuário usa manualmente a "check for updates" para forçar o código de volta, caso contrário, a versão não será visível até que o cache expire.
  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        setInfo(r);
        if (!quiet) {
          if (r.error) toast.error("Falha ao verificar atualizações: " + r.error);
          else if (r.has_update) toast.success(`Nova versão encontrada: ${r.latest}`);
          else if (r.comparable) toast.success("A versão atual já é a mais recente");
        }
      })
      .catch((e) => {
        if (!quiet) toast.error("Falha ao verificar atualizações: " + (e as Error).message);
      })
      .finally(() => setChecking(false));
  }, []);

  React.useEffect(() => {
    check(true);
  }, [check]);

  // Rodada / api/health até que o número da versão mude.
  //
  // O veredicto deve ser "a versão mudou" em vez de "a que pode ser conectada": a versão antiga será re-estabelecida brevemente durante o processo de re-ajustamento.
  // (Nessa altura, só era para o artex.new ser substituído e depois sair imediatamente)
  const waitForNewVersion = React.useCallback(async (fromVersion: string) => {
    setRestarting(true);
    const deadline = Date.now() + RESTART_TIMEOUT_MS;
    while (Date.now() < deadline) {
      await sleep(2000);
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (r.ok) {
          const j = (await r.json()) as { version?: string };
          if (j.version && j.version !== fromVersion) {
            toast.success(`Atualizado para ${j.version}; recarregando a página`);
            await sleep(800);
            window.location.reload();
            return;
          }
        }
      } catch {
        // A reabertura das janelas não está ligada como esperado, por isso continue a rotação.
      }
    }
    setRestarting(false);
    toast.error("Tempo esgotado ao aguardar o serviço. Verifique os logs e o uso de start.sh/start.bat.");
  }, []);

  // Assinatura para atualizar o progresso. SSE não vai / api reescrever (os buffers de camada e eventos não podem ser empurrados para fora).
  const openStream = React.useCallback(
    (fromVersion: string) => {
      const es = new EventSource(sseUrl("/api/update/stream"));
      es.onmessage = (ev) => {
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          es.close();
          setBusy(false);
          toast.error("Falha na atualização: " + (p.error || p.message));
          return;
        }
        if (p.phase === "staged") {
          es.close();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        // O SSE deve ser desligado quando o processo terminar. Se tiver entrado à espera de reiniciar, é normal.
        // O pedido de aconselhamento adicional deve ser encaminhado para o / api/health.
        es.close();
      };
      return es;
    },
    [waitForNewVersion],
  );

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `Confirma a atualização para ${info.latest}?\n\n` +
        "O programa será reiniciado e as tarefas em execução serão interrompidas.\n" +
        (info.mode === "docker"
          ? "\nNo container, a atualização não troca ferramentas como Playwright ou Nmap; " +
            "use docker compose pull quando necessário."
          : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: "Preparando…" });
    const es = openStream(from);
    api.applyUpdate().catch((e) => {
      es.close();
      setBusy(false);
      setProgress(null);
      toast.error("Não foi possível iniciar a atualização: " + (e as Error).message);
    });
  };

  const doRollback = () => {
    if (!info) return;
    if (
      !window.confirm(
        "Voltar para a versão anterior?\n\nO programa será reiniciado e as tarefas serão interrompidas.\nO banco de dados não será revertido.",
      )
    )
      return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success("Versão anterior restaurada; reiniciando…");
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error("Falha no rollback: " + (e as Error).message);
      });
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // Somente a porcentagem real pode ser obtida na etapa de download (com base no Content-Length). Verifique/switch/esperar para reiniciar
  // O longo período, desconhecido, as tiras de progresso preenchido e uma pulsação animada significa "no meio de um ocupado, mas não por muito tempo".
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;

  return (
    // Define a página como um layout de fluxo de cachoeira multi-linha, com o cartão responsável pelo espaçamento de linha e proibindo quebras de barra cruzada (ver comentário page.tsx).
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          Versão e atualização
        </CardTitle>
        <CardDescription>
          Consulta e instalação pelo GitHub, desativadas por padrão no perfil corporativo.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">Versão atual</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : "Binário"}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">Versão mais recente</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:underline"
            >
              Notas da versão <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.disabled && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.reason}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            Não foi possível acessar o GitHub: {info.error}. Configure o proxy global e tente novamente.
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-xs text-muted-foreground">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />A versão {info.latest} não oferece pacote para{" "}
            {info.os}/{info.arch} ({info.asset}).
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-xs text-muted-foreground">
            O pacote <span className="font-mono">{info.asset}</span>
            {info.size ? ` (${humanSize(info.size)})` : ""} será validado por SHA256 antes da troca.
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />A versão atual já é a mais recente.
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-xs text-muted-foreground">
            No Docker, a atualização troca apenas o programa, não ferramentas como Playwright ou Nmap.
            <span className="font-mono"> docker compose up -d </span>
            recria o container com a versão da imagem. Para atualizar a imagem, execute
            <span className="font-mono"> docker compose pull artex &amp;&amp; docker compose up -d artex</span>。
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-xs text-muted-foreground">
              {restarting ? "Reiniciando e aplicando a nova versão; a página será recarregada…" : progress?.message}
            </p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button
            variant="outline"
            size="sm"
            onClick={() => check(false)}
            disabled={checking || busy || restarting || info?.disabled}
          >
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            Verificar atualizações
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || info?.disabled || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? `Atualizar para ${info.latest}` : "Atualizar agora"}
          </Button>
          {info?.has_backup && !info.disabled && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              Voltar à versão anterior
            </Button>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          A atualização depende do supervisor. Inicie pelo <span className="font-mono">start.sh</span> (ou
          <span className="font-mono"> start.bat</span> no Windows).
        </p>
      </CardContent>
    </Card>
  );
}
