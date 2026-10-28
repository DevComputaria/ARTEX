# Implantação empresarial local

## Arquitetura recomendada

Execute o frontend/backend e o proxy de captura em uma zona de testes segregada. Mantenha PostgreSQL em rede privada, exponha a interface somente por HTTPS atrás do proxy reverso corporativo e permita saída apenas para destinos aprovados.

## Controles mínimos

1. Use SSO ou um proxy de autenticação corporativo na frente do ARTEX; a autenticação local deve permanecer como segunda barreira.
2. Guarde chaves de LLM, SMTP, MCP e webhooks em um cofre de segredos.
3. Não publique as portas `8787`, `8788` ou `5432` diretamente na Internet.
4. Use uma conta de serviço sem privilégios, filesystem somente leitura quando possível e volumes separados para `data` e `skills`.
5. Defina allowlists de egress no firewall; não confie apenas nos controles de escopo da aplicação.
6. Centralize logs com redaction e limite o acesso às evidências.
7. Faça backup cifrado do PostgreSQL e de `data`; teste a restauração.

## Perfil sem Internet

Mantenha:

```dotenv
ARTEX_SELF_UPDATE=0
```

Use um endpoint LLM interno. Espelhe módulos Go, pacotes npm, imagens Docker e ferramentas de segurança em repositórios corporativos. Construa os artefatos em CI e transfira somente versões assinadas para produção.

## Proxy reverso

O tráfego SSE exige buffering desativado e timeout longo:

```nginx
location / {
    proxy_pass http://127.0.0.1:8787;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;
    proxy_cache off;
    proxy_read_timeout 3600s;
    proxy_set_header Connection "";
}
```

Termine TLS com certificados corporativos e aplique cabeçalhos de segurança no proxy.

## Homologação

- criar uma tarefa somente contra laboratório isolado;
- validar aprovação humana para ações destrutivas;
- confirmar captura e expurgo de tráfego;
- observar DNS e egress durante toda a execução;
- testar parada, reinício, backup e restauração;
- verificar retenção e mascaramento de segredos;
- registrar aceite de Segurança, Infraestrutura, Jurídico e responsável pelo pentest.

## Atualização controlada

O fluxo recomendado é: importar upstream em branch separada, revisar diferenças, executar SAST/SCA e testes, gerar SBOM, assinar imagem/binário e promover por ambientes. A atualização direta pelo GitHub deve permanecer desativada em produção.
