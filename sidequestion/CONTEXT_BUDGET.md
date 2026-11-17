# Conversas paralelas longas e orçamento de contexto

Correção de 2026-09-11. A implementação original tratava o número de caracteres do JSON como tokens e herdava a reserva de saída de 32K da tarefa principal. Isso rejeitava perguntas válidas quando havia muito HTML, JavaScript ou resultados de ferramentas.

## Comparação com implementações abertas

- [Contexto paralelo do Grok CLI](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): extrai trechos recentes do usuário e do assistente, com cerca de 2.000 caracteres e no máximo 400 por item; não mantém uma conversa paralela contínua nesse caminho.
- [Solicitação independente do Grok CLI](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): usa cancelamento independente, limita a saída a 2.048 tokens quando suportado e não oferece ferramentas.
- [Compressão principal do Grok CLI](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estima tokens, mantém conteúdo recente, atualiza um resumo incremental e trata cortes entre turnos.
- [Compressão de sessão do OpenCode](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): estima a requisição completa, reserva saída e margem, combina conteúdo recente com resumo incremental e resume sem ferramentas. Os padrões são 8.000 tokens recentes e até 4.096 de saída do resumo.
- [Recuperação de estouro no OpenCode](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): só tenta recuperar antes de o assistente produzir saída; a chamada recuperada não entra novamente no mesmo caminho.

O ARTEX adotou orçamento de saída independente, conteúdo recente, resumo incremental e recuperação limitada. As mensagens estruturadas e o pareamento de ferramentas do norma v0.3.6 são mantidos; eventos de compressão não são escritos no transcript principal.

## Orçamento e execução

- As mensagens usam a estimativa do norma por bytes UTF-8 de cada bloco, com margem de 4/3. System prompt, schemas de ferramentas e envelopes também contam. Não é uma contagem exata do tokenizer do modelo.
- A saída paralela permite por padrão até 8.192 tokens, sem exceder o limite configurado no perfil principal. `ARTEX_BTW_MAX_OUTPUT_TOKENS` aceita de 256 a 32768 sem alterar o modelo padrão nem os parâmetros da tarefa.
- O orçamento de entrada é a janela de contexto menos saída e margem de segurança. Uma janela desconhecida usa 200K. A margem é 5% da janela, limitada entre 128 e 8.192 tokens.
- Até 20 pares bem-sucedidos são carregados por lote e preservados literalmente. Eles usam no máximo 1/4 do orçamento de entrada e nunca mais de 16K tokens.
- Pares excedentes alimentam um resumo incremental com origem e horário. Respostas históricas não substituem evidências novas de ferramentas; em conflito, vale o snapshot principal mais recente.
- Se o contexto ainda for grande, somente mensagens antigas da cópia são resumidas; até 8K tokens recentes são mantidos. O corte não separa uma chamada de ferramenta do resultado.
- A entrada do resumo é dividida com segurança em UTF-8 conforme a janela restante. A saída é limitada a 2.048 tokens. Resumo vazio, truncado, com chamadas de ferramenta ou acima do orçamento não entra no cache. Cada pergunta permite até 12 chamadas de resumo dentro do mesmo timeout de 120 segundos.
- Se a primeira resposta indicar estouro antes de qualquer texto ou ferramenta, o contexto pode ser reduzido e tentado uma vez. Ausência de redução encerra a recuperação. Outros erros e saídas parciais não disparam nova tentativa.
- Todo uso recebido, inclusive resumos, falhas e cancelamentos, é acumulado na mesma solicitação. Quando o Provider não informa uso, registra-se zero; a estimativa nunca é apresentada como uso real.

## Persistência e interface

`side_question_sessions.memory` armazena o resumo de perguntas antigas, o ordinal coberto e o resumo do contexto principal indexado pela identidade do snapshot. `side_question_requests.context_info` guarda a fase de preparação, quantidade reproduzida, uso de resumo e estimativas de orçamento.

O resumo só é salvo se a solicitação continuar ativa e a versão de limpeza coincidir. Limpar também remove o cache, e gravações atrasadas não restauram dados. Um snapshot novo não reutiliza o resumo do antigo. Os campos integram o arquivo v3; restaurações de v3 antigo recebem um objeto vazio e v1/v2 continuam compatíveis.

O POST aceita e retorna a solicitação antes da preparação. Compressão ocorre em background sem manter lock de admissão nem transação. SSE e histórico exibem as fases de preparação, organização, compressão e resposta. Falha de compressão é persistida como estado terminal. A interface preserva o erro e restaura o rascunho, sem toast que cubra o campo de entrada.

## Registro de validação

- Reproduções com 19, 20, 21 e 50 pares, preservação de conclusões antigas e reutilização do cache após reinício: passaram.
- Respostas longas, contexto de código, divisão por orçamento, pareamento de ferramentas, snapshot imutável e invalidação por snapshot novo: passaram.
- Falha, cancelamento, truncamento, retorno de ferramenta, corrida de limpeza, limite de chamadas, recuperação única e não repetição após stream parcial: passaram.
- Paginação e reinício no PostgreSQL isolado, arquivos v1/v2/v3, restauração de metadados e compartilhamento de quatro vagas por 20 sessões pai: passaram.
- Build Go, TypeScript, Biome nos componentes alterados e build de produção Next.js em diretório isolado: passaram.
- A interface foi verificada com fixture em 1280×720 e 390×844, sem overflow horizontal nem erros no console.
- O replay somente leitura de um snapshot existente de 293.085 caracteres deixou de ser recusado pela contagem local; nenhum modelo externo foi chamado nesse teste.
- O envio de um snapshot privado real ao Grok foi recusado pela aprovação automática e não conta como teste aprovado.

Comandos de validação, sempre com banco descartável:

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/artex
npx tsc --noEmit
npm run build -- --webpack
```

O build de produção do frontend usou uma cópia isolada para não sobrescrever `.next` da prévia. Os caminhos históricos de validação foram `/private/tmp/artex-btw-budget-candidate` e `/private/tmp/artex-btw-preview/artex`.
