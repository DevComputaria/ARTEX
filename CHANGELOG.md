# Registro de alterações

Este arquivo registra as mudanças da edição corporativa em português. O histórico completo do projeto original permanece disponível no repositório upstream e no histórico Git anterior ao fork.

## Não publicado

### Segurança e privacidade

- atualização automática pelo GitHub alterada para opt-in por meio de `ARTEX_SELF_UPDATE=1`;
- verificação de versão, download e bootstrap de substituição de binário bloqueados por padrão;
- link externo de suporte removido da barra lateral;
- inventário de telemetria e fluxos de saída documentado;
- orientações de segmentação, segredos, retenção e implantação sem Internet adicionadas.

### Localização

- README e documentação operacional traduzidos para português do Brasil;
- idioma raiz da interface definido como `pt-BR`;
- navegação principal, login, metadados e atualização localizados;
- comentários novos e código alterado documentados em português.

### Implantação

- `ARTEX_SELF_UPDATE=0` incluído no `.env.example` e no Docker Compose;
- recomendações para LLM interno, proxy reverso, firewall de saída e pipeline de atualização controlada.

## Base upstream

- commit: `4379d69`;
- licença preservada: GNU Affero General Public License v3.0.
