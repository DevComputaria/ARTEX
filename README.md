<div align="center">

# ARTEX — edição corporativa em português

Sistema autônomo de testes de intrusão com múltiplos agentes de IA, backend em Go e interface em Next.js.

[![Licença: AGPL-3.0](https://img.shields.io/badge/licen%C3%A7a-AGPL--3.0-blue)](LICENSE)

</div>

O ARTEX coordena um planejador e agentes de execução que compartilham grafos de ativos e de exploração. A plataforma registra o tráfego, mantém aprovação humana para ações sensíveis e pode ser executada como binário único ou com Docker Compose.

> Use somente em ativos para os quais sua organização possua autorização formal. Testes de intrusão podem causar indisponibilidade ou perda de dados.

## O que mudou nesta edição

- documentação operacional e mensagens principais em português do Brasil;
- perfil corporativo sem consulta automática a fornecedores;
- atualização via GitHub desativada por padrão (`ARTEX_SELF_UPDATE=0`);
- inventário documentado de todos os fluxos de rede externos;
- instruções para execução local, segmentação de rede, segredos e retenção de dados.

Consulte [Auditoria de telemetria](docs/AUDITORIA_TELEMETRIA.md) e [Implantação empresarial local](docs/IMPLANTACAO_EMPRESARIAL.md) antes de colocar o sistema em produção.

## Capturas de tela

| Painel | Tarefas |
| :---: | :---: |
| ![Painel](screenshots/dashboard.png) | ![Tarefas](screenshots/tasks.png) |

| Sessões e ferramentas | Grafo de exploração |
| :---: | :---: |
| ![Sessões](screenshots/sessions.png) | ![Grafo](screenshots/graph.png) |

| Achados | Ativos |
| :---: | :---: |
| ![Achados](screenshots/findings.png) | ![Ativos](screenshots/assets.png) |

| Tráfego | Aprovações e conversa |
| :---: | :---: |
| ![Tráfego](screenshots/traffic.png) | ![Conversa](screenshots/chat.png) |

## Requisitos

- Docker com Compose, ou Go e Node.js para compilação local;
- PostgreSQL 16 ou compatível;
- um provedor LLM local ou explicitamente autorizado pela empresa;
- pelo menos 4 GB de RAM para uso básico; tarefas concorrentes exigem mais recursos.

## Instalação recomendada com Docker Compose

```bash
git clone <URL-DO-REPOSITORIO-CORPORATIVO> artex
cd artex
cp .env.example .env
```

Edite `.env`, troque obrigatoriamente `POSTGRES_PASSWORD` e mantenha a atualização externa desativada:

```dotenv
POSTGRES_PASSWORD=use-um-segredo-forte
ARTEX_SELF_UPDATE=0
```

Inicie os serviços:

```bash
docker compose up -d
docker compose ps
```

Acesse `http://localhost:8787`. No primeiro acesso, a rota `/setup` solicitará a senha do administrador.

O proxy de captura escuta somente em `127.0.0.1:8788` no Compose fornecido. Não exponha essa porta publicamente.

## Compilação local

```bash
cd web
npm ci
npm run build:static
cd ..
cp -r web/out server/webui/dist
CGO_ENABLED=0 go build -tags embedui -o artex ./cmd/artex
./start.sh
```

O frontend usa fontes locais e não precisa carregar recursos do Google Fonts em tempo de execução.

## Configuração do banco

Use `ARTEX_PG_DSN` ou `config.json`:

```json
{
  "database": {
    "host": "127.0.0.1",
    "port": 5432,
    "user": "artex",
    "password": "troque-esta-senha",
    "dbname": "artex",
    "sslmode": "disable"
  }
}
```

Em produção, prefira TLS no PostgreSQL e injete a senha por cofre de segredos, não por arquivo versionado.

## Configuração do LLM

O ARTEX não funciona de forma autônoma sem um LLM. Para manter os dados na empresa, aponte o formato compatível com OpenAI para um endpoint interno:

```dotenv
ARTEX_LLM_PROVIDER=openai
ARTEX_LLM_MODEL=modelo-interno
ARTEX_LLM_BASE_URL=https://llm.interno.exemplo/v1
OPENAI_API_KEY=<segredo-do-cofre>
```

Também é possível configurar Anthropic ou outro endpoint compatível pela interface. Ao usar um provedor externo, prompts, contexto, resultados das ferramentas e possíveis dados sensíveis podem sair da rede corporativa.

## Atualizações

Esta edição não consulta o GitHub automaticamente. O endpoint de verificação retorna apenas o estado local enquanto `ARTEX_SELF_UPDATE` não estiver habilitado.

Para ambientes conectados que aceitem esse risco:

```dotenv
ARTEX_SELF_UPDATE=1
```

Mesmo nesse caso, recomenda-se promover imagens ou binários por um pipeline interno, com SBOM, assinatura e aprovação, em vez de atualizar diretamente em produção.

## Integrações que podem gerar tráfego externo

Todas dependem de configuração ou ação explícita:

- provedor LLM;
- servidores MCP e sincronização ScopeSentry;
- Brave Search e Tavily;
- notificações por webhook, Telegram, DingTalk, Feishu, WeCom e SMTP;
- alvos dos testes de intrusão e ferramentas executadas pelos agentes;
- atualização pelo GitHub, somente quando `ARTEX_SELF_UPDATE=1`.

Não foram encontrados SDKs de analytics, rastreamento de usuário, Sentry, OpenTelemetry ou envio oculto de métricas no código analisado.

## Testes

```bash
go test ./...
cd web
npm ci
npm run lint
npm run build:static
```

Alguns testes Go precisam de PostgreSQL. Veja as mensagens da suíte para configurar a conexão de teste.

## Dados persistidos

- PostgreSQL: configuração, tarefas, achados, conversas e metadados;
- `./data`: chave JWT, bancos auxiliares, evidências e registros locais;
- `./skills`: skills carregadas pelos agentes.

Defina retenção, backup cifrado e descarte seguro para esses três conjuntos. Evidências de pentest podem conter tokens, cookies, credenciais e dados pessoais.

## Licença

O código permanece sob a licença GNU Affero General Public License v3.0. A disponibilização do serviço em rede pode exigir que o código-fonte correspondente, inclusive modificações, seja oferecido aos usuários do serviço. Valide as obrigações com a área jurídica antes da implantação corporativa.
