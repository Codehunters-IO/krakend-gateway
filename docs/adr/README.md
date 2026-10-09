# Architectural Decision Records

Registro de decisiones arquitectónicas del gateway. Formato [MADR](https://adr.github.io/madr/).

## Ciclo de vida

`proposed` → `accepted` → (`deprecated` | `superseded`). Los ADR `accepted` son inmutables:
para cambiar una decisión, escribir uno nuevo con `supersedes: NNNN`.

## Índice

El número 0006 (autorización por app en el edge) está reservado: el plan y
`scripts/smoke-security-headers.sh` lo referencian por nombre antes de que exista.

La columna **Fecha** es la de redacción (`date` en el frontmatter), no la de aceptación.
Para ADR-0001 hay tres meses entre las dos: escrito el 2026-06-26, aceptado el 2026-09-25.

| ADR | Título | Fecha de redacción | Estado |
|-----|--------|--------|-------|
| [0001](0001-security-headers-edge.md) | Cabeceras de seguridad en el edge vía `security/http` | 2026-06-26 | accepted |
| [0002](0002-platform-edge-global-keycloak-realm-per-product.md) | Edge de plataforma compartido + Keycloak global con realm-por-producto | 2026-08-02 | superseded por 0005 |
| [0003](0003-session-state-at-the-edge.md) | Estado de sesión en el edge — cookie opaca resuelta contra Valkey | 2026-10-05 | accepted |
| [0004](0004-plugin-chain-order.md) | Array `plugin/http-server` invertido, y los dos plugins de auth juntos | 2026-10-06 | accepted |
| [0005](0005-one-platform-realm-and-master-as-operator-realm.md) | Un realm de aplicaciones (`codehunters`) y `master` como realm de operadores | 2026-10-07 | proposed |
| [0007](0007-one-gateway-per-realm.md) | Un gateway por realm, no un gateway multi-issuer | 2026-10-09 | proposed |
| [0008](0008-the-edge-keeps-its-own-jwt-plugin.md) | El edge se queda con su propio plugin JWT, no con el validador declarativo | 2026-10-09 | accepted |
