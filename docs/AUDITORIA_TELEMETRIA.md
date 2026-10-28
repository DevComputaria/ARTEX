# Auditoria de telemetria e tráfego externo

Data da revisão: 28 de outubro de 2026. Base analisada: commit upstream `4379d69`.

## Conclusão

Não foi encontrada telemetria oculta. O projeto não inclui SDKs de analytics, rastreamento de sessão, Sentry, Datadog, New Relic, OpenTelemetry, Prometheus remoto ou mecanismos equivalentes de envio automático de métricas.

Foi encontrado um contato automático com o GitHub para verificar releases. Nesta edição ele foi convertido em recurso opt-in: sem `ARTEX_SELF_UPDATE=1`, o backend não consulta a API do GitHub, não baixa releases e não executa o bootstrap de troca de binário.

## Fluxos de rede identificados

| Fluxo | Destino | Quando ocorre | Dados potencialmente enviados | Situação local |
| --- | --- | --- | --- | --- |
| Atualização | GitHub e armazenamento de releases | Verificação/instalação de versão | versão, SO/arquitetura, IP e cabeçalhos HTTP | desativado por padrão |
| LLM | URL configurada | chamadas dos agentes e aprovações por modelo | prompts, contexto, ferramentas, achados e uso de tokens | exige configuração |
| Busca | Brave/Tavily | quando a integração é configurada e usada | consultas de busca | exige chave/configuração |
| MCP | URL configurada | descoberta e chamada de ferramentas | argumentos e resultados MCP | exige configuração |
| ScopeSentry | MCP configurado | sincronização solicitada | filtros, projetos, tarefas e chave no cabeçalho | exige configuração |
| Notificações | webhook, Telegram, DingTalk, Feishu, WeCom ou SMTP | entrega habilitada | dados dos achados e links internos | exige configuração |
| Pentest | ativos autorizados | execução de ferramentas/agentes | tráfego de teste definido pela tarefa | função principal |
| Interface | link externo de suporte | clique do usuário | IP e metadados do navegador | removido nesta edição |

## Evidências no código

- `selfupdate/github.go`: consulta e download de releases do GitHub;
- `server/update.go`: endpoints de verificação, aplicação e rollback;
- `agent/provider.go` e `server/server_mgmt.go`: clientes de LLM configuráveis;
- `mcphttp/client.go`: transportes MCP HTTP e SSE;
- `server/sync_scopesentry.go`: sincronização explícita com ScopeSentry;
- `notify/`: canais de notificação e controles contra SSRF/vazamento de credenciais;
- `enrich/enrich.go`: requisições de enriquecimento aos próprios ativos analisados;
- `traffic/`: proxy de captura e tráfego funcional de pentest.

## Riscos residuais

1. Um agente pode executar ferramentas de rede contra destinos permitidos pela tarefa. Controles de escopo da aplicação não substituem firewall de saída.
2. Provedores LLM externos recebem conteúdo operacional sensível. Prefira endpoint interno e revise logs do provedor.
3. Webhooks e MCPs são destinos arbitrários configurados por administradores. Restrinja egress por allowlist.
4. Dependências são baixadas durante a compilação (`go mod`, `npm`) e imagens durante o deploy. Espelhe artefatos internamente para builds isolados.
5. O Compose original usa imagem pública. Para produção, construa, assine e publique em registry corporativo.
6. Durante esta revisão, `npm ci` relatou divergências de integridade em diversos tarballs obtidos pelo proxy do ambiente. A instalação foi interrompida e nenhuma verificação foi ignorada. Reexecute com um registry corporativo confiável e investigue qualquer divergência antes de gerar artefatos.

## Recomendação de validação em produção

- executar o container em uma VLAN sem saída geral para Internet;
- liberar apenas PostgreSQL, LLM interno, alvos autorizados e integrações aprovadas;
- registrar consultas DNS e conexões de saída durante um teste de aceitação;
- confirmar que `ARTEX_SELF_UPDATE=0` está efetivo;
- desabilitar integrações não usadas e rotacionar todas as chaves após homologação.
