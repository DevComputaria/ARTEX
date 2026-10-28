# Evidências de tráfego associadas a achados

## Objetivo

O ARTEX pode vincular requisições e respostas capturadas a um achado de segurança. O vínculo permite reproduzir a análise, revisar a evidência usada pelo agente e manter histórico quando o relatório é atualizado.

## Modelo de dados

- o tráfego bruto permanece no armazenamento de tráfego da tarefa;
- o achado guarda referências estáveis para as trocas selecionadas;
- uma versão de evidência identifica alterações na seleção;
- a versão do relatório registra qual conjunto de evidências foi usado na geração do texto;
- a remoção de uma tarefa deve eliminar os vínculos e os arquivos pertencentes a ela.

O vínculo não deve duplicar corpos grandes no PostgreSQL. Metadados, hashes e referências ficam no banco; requisições e respostas são lidas do arquivo de captura quando necessário.

## Fluxo

1. O agente registra ou atualiza um achado.
2. O backend valida se cada ID de tráfego pertence à mesma tarefa.
3. As referências válidas são persistidas em ordem determinística.
4. A API de detalhe entrega metadados e prévias paginadas dos corpos.
5. Exportações devem incluir a versão e os hashes necessários para verificação.

## Requisitos de segurança

- nunca aceitar referência de tráfego pertencente a outra tarefa;
- impedir traversal ao resolver caminhos de arquivos;
- limitar tamanho, offset e quantidade de evidências por resposta;
- identificar conteúdo binário e não renderizá-lo como HTML;
- aplicar autorização de tarefa antes de retornar metadados ou corpos;
- mascarar cookies, tokens e credenciais nas visualizações e exportações quando a política exigir;
- registrar falhas de integridade sem incluir o segredo nos logs.

## Retenção

Evidências podem conter dados pessoais e segredos. A organização deve definir prazo de retenção, criptografia em repouso, acesso por função, backup e descarte seguro. A exclusão de uma tarefa precisa abranger banco, arquivos de captura, anexos e cópias de exportação sob controle da aplicação.

## Testes esperados

- criação, substituição e remoção de vínculos;
- rejeição de IDs inexistentes ou de outra tarefa;
- leitura paginada e limites de tamanho;
- detecção de binário e conteúdo truncado;
- integridade após arquivamento e restauração;
- limpeza concorrente durante exclusão de tarefa;
- compatibilidade com registros criados antes da introdução do versionamento.
