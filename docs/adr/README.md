# Architectural Decision Records

Registro de decisiones arquitectónicas del gateway. Formato [MADR](https://adr.github.io/madr/).

## Ciclo de vida

`proposed` → `accepted` → (`deprecated` | `superseded`). Los ADR `accepted` son inmutables:
para cambiar una decisión, escribir uno nuevo con `supersedes: NNNN`.

## Índice

| ADR | Título | Estado | Fecha |
|-----|--------|--------|-------|
| [0001](0001-security-headers-edge.md) | Cabeceras de seguridad en el edge vía `security/http` | proposed | 2026-06-26 |
| [0002](0002-platform-edge-global-keycloak-realm-per-product.md) | Edge de plataforma compartido + Keycloak global con realm-por-producto | proposed | 2026-08-02 |
