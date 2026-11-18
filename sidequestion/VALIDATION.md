# Registro de validação do `/btw`

Data: 2026-09-10. Branch original: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

Os testes usaram banco PostgreSQL e diretório de dados isolados. Credenciais reais de modelos foram injetadas apenas no ambiente descartável, não foram gravadas no código ou neste documento e não alteraram o modelo padrão do produto. Versões: Go 1.26.3, norma v0.3.6 e Next.js 16.2.9.

Conversas reais, objetos retornados, asserções e a revisão original do Qwen estão em [validation-2026-09-10.json](validation-2026-09-10.json), sem credenciais de API.

## Verificações de engenharia

| Escopo | Resultado | Evidência |
| --- | --- | --- |
| Cópia profunda de mensagens e parâmetros de ferramentas | Passou | `TestCheckpointDeepCopyAndBoundaries` |
| Resumo não sobrescreve snapshot; resposta completa e estado terminal publicam; fragmentos são excluídos | Passou | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Identidade do membro efetivo do pool | Passou | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Pareamento de ferramentas, 20 pares, corte por orçamento e erro de limite | Passou | `TestBuildRequestCompactionToolPairingAndBudget` |
| Fluxo principal e paralelo simultâneos, com cancelamentos isolados | Passou | `TestMainSideConcurrencyAndIndependentCancellation` |
| Sem execução de ferramentas; streaming e resposta atômica; uso em falha | Passou | `TestServiceNoToolsAndUsageOnFailure` |
| ChatAgent real do norma, ferramenta Read local e isolamento de transcript/atividade | Passou | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation` |
| Persistência, paginação, idempotência e recuperação parcial | Passou | `TestSideHistoryIdempotencyPagingAndRecovery` |
| Limpeza contra gravações atrasadas e exclusão do pai | Passou | `TestSideClearLateWritersAndDeletedParent` |
| Arquivo e restauração MainAgent/Worker v1, v2 e v3 | Passou | `TestSideTaskArchiveVersions` |
| Três APIs pai, autenticação, propriedade e exclusão lógica | Passou | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion` |
| SSE reconectável, cancelamento e limpeza com sessão principal ocupada | Passou | `TestSideHTTPBusyIsolationClearAndReconnect` |
| Uma solicitação por pai e quatro globais | Passou | testes `TestSideHTTP…` |
| Snapshot persistido antes da admissão e continuação após reinício | Passou | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Rejeição de perfil em cache removido ou alterado | Passou | `TestSideRejectsDeletedOrChangedCachedProfile` |
| Cancelamento antes do arquivo e persistência final de uso | Passou | `TestSideTaskDrainPersistsBeforeArchive` |
| Uso gravado uma vez quando o consumidor interrompe o stream | Passou | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| Worker restaurado continua publicando snapshots | Passou | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| TypeScript, build de produção e Biome dos módulos novos | Passou | comandos abaixo |

Use sempre um banco descartável em `ARTEX_PG_DSN`; nunca aponte estes testes para produção:

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

Na baseline, dois testes existentes do pacote `server` falharam durante a limpeza de diretórios temporários com `directory not empty`: `TestInheritedActivityDetailAndRelationDeletion` e `TestTaskMetadataPatchReturnsRenameAndPin`. A mesma falha foi reproduzida sem as alterações do recurso. A baseline também falhou na quantidade de goals de `TestCoreTaskLifecyclePG`; a versão final não apresentou essa falha. Esses problemas não foram marcados como aprovados nem ocultados por mudanças de asserção.

## Verificação no navegador

O navegador integrado foi conectado a serviços Go e Next.js isolados. Em desktop e 390×844 foram verificados:

- pergunta `/btw` simultânea à conversa principal;
- perguntas subsequentes e preservação de resposta parcial ao interromper;
- continuação ao fechar o painel e recuperação após atualizar a página;
- Drawer móvel sem overflow horizontal;
- confirmação de limpeza sem apagar transcript ou snapshot principal;
- troca entre MainAgent e dois Workers sem mistura de históricos;
- Worker principal permanecendo ativo após cancelar somente a pergunta paralela;
- console sem erros ou avisos.

Fixtures controladas foram usadas para validar a ordem de concorrência sem depender da velocidade de um modelo real.

## Conversas com modelo real

O teste priorizou `grok-4.6` por uma API compatível com OpenAI em `http://127.0.0.1:12580/tingly/openai`. A sondagem retornou HTTP 200, modelo `grok-4.6` e `READY` em 2,82 s. Como o modelo preferencial estava disponível, os fallbacks `glm` e `glm-5.3` não foram validados.

| Cenário | Resultado real |
| --- | --- |
| Pergunta sobre ativo, objetivo e marcador durante a sessão principal | Retornou `redhaze.top`, o objetivo de leitura/resumo e `BTW-REAL-0910`; 16,97 s |
| Pergunta sobre evidência das ferramentas | Citou WebFetch 200, redirecionamentos curl 301 → 302 → 200 e o título; 7,24 s |
| Pedido para criar arquivo via Bash | Recusado; arquivo não foi criado; 7,74 s |
| Efeito no contexto principal | SHA-256 do transcript e atividades permaneceram iguais; zero ferramentas paralelas |
| Reinício real do serviço Go | Três itens do histórico foram preservados e o snapshot respondeu sem executar novamente o Agent |
| Configuração Grok sem streaming | Resposta correta para `ATOMIC-0910`; uso salvo: input 11734, output 138, cache_read 11520 |

A sessão principal consultou uma página pública com WebFetch e curl e salvou temporariamente a resposta apenas no ambiente local. Isso é separado da comprovação de que o fluxo paralelo não executou ferramentas. SHA-256 do transcript principal: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

O endpoint Tingly não devolveu `usage` no streaming, mesmo com `stream_options.include_usage=true`: HTTP 200, 12 frames e nenhum frame de uso. Portanto, zero nesses testes significa ausência da métrica, não ausência de cobrança. Uso não-streaming, falhas e cancelamentos das fixtures foram persistidos corretamente.

## Revisão Qwen

`qwen-flash`, por `https://dashscope.aliyuncs.com/compatible-mode/v1`, recebeu as três primeiras conversas reais, evidências e asserções. Retornou `verdict: accept` e `concerns: []`; uso: prompt 6625, completion 312, total 6937.

A revisão não incluiu os testes posteriores de reinício e resposta não-streaming. A afirmação de que não houve gravação era ampla demais, porque a sessão principal criou um arquivo temporário local. Concorrência, zero ferramentas e isolamento do transcript foram comprovados pelas asserções; a revisão do modelo foi apenas complementar.
