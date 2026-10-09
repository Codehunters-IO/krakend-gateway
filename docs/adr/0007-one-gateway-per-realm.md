---
status: proposed
date: 2026-10-09
decision-makers: [Carlos Andres Montoya Tobon]
consulted: [equipo plataforma, seguridad]
informed: [equipo frontend, equipo forgeos, auth-bff]
---

# ADR-0007: Un gateway por realm, no un gateway multi-issuer

## Context and Problem Statement

La pregunta que origina este ADR era otra: **¿puede el edge validar tokens de varios
realms?** El diseño obvio —leer el claim `iss` y construir con él la ruta del JWKS— se
descartó por ser un bypass de autenticación y no una optimización: en ese punto el token
todavía no está verificado, así que usar uno de sus propios claims para decidir con qué clave
verificarlo es circular. Un atacante firma con su clave, publica su JWKS, pone el `iss`
apuntando ahí, y la firma valida. De paso, el borde hace una petición HTTP a una URL que
elige el cliente.

La forma correcta de leer el `iss` es como **clave de búsqueda en una allowlist cerrada**, no
como plantilla de URL. Pero incluso haciéndolo bien, un solo gateway multi-issuer arrastra
seis problemas medidos en este repositorio el 2026-10-09:

| Problema | Dónde |
|---|---|
| `jwksReady` es un flag global: un IdP caído es `503` para todo el tráfico, incluido el del realm sano | `plugins/jwt-headers/main.go:169` |
| `claims_to_headers` no lleva procedencia de realm: `x-user-roles` de dos realms llega indistinguible | `config/settings/jwt.json` |
| `sub` es único por realm, no global: `x-user-id` deja de ser una clave | — |
| `introspection_client_id` es uno solo: contra el segundo realm la llamada falla y el código **permite el token** (`main.go:285`), así que la revocación deja de existir en silencio | `main.go:280-285` |
| El contrato `v1:session:{sid}` no tiene campo de realm | `docs/session-flow.md` |
| El atajo de vaciar `issuer` para «aceptar varios» desactiva la comprobación de `iss` por completo | `main.go:242` |

## Decision Drivers

- **Aislar dominios de seguridad con procesos cuesta menos que aislarlos con código.** Cinco
  de los seis problemas de arriba desaparecen por construcción, sin tocar un plugin.
- **Dirección de la dependencia.** Es el mismo argumento que ADR-0003: Valkey es
  infraestructura, `auth-bff` es una aplicación que vive detrás de este gateway. Un gateway
  por realm no invierte ninguna capa.
- **Radio de explosión y ciclo de vida.** Un despliegue por realm cae solo y se despliega
  solo.
- **Mínimo privilegio sobre el almacén.** Con la ACL de Valkey (ADR-0003, cerrado el
  2026-10-09), un gateway por realm puede tener su propio usuario ACL y su propio prefijo de
  clave. Un borde comprometido no alcanza las sesiones del otro realm.

## Considered Options

1. **Un gateway por realm**, una sola imagen, configuración por entorno.
2. **Un gateway por realm, una imagen por realm**, con los settings horneados.
3. **Un solo gateway multi-issuer** con allowlist `iss` → JWKS.
4. **Federación dentro de Keycloak**: el IdP ajeno se registra como Identity Provider del
   realm de plataforma y el edge sigue viendo un issuer.

## Decision Outcome

Elegida: **la opción 1**, con la **opción 4 como preferida cuando sea aplicable**.

Si lo que aparece es otra **autoridad de identidad** —el Keycloak o el AD de un cliente—, lo
normal no es multi-issuer ni un segundo gateway: es federación dentro de `codehunters`. El
edge sigue viendo un issuer, un JWKS y un vocabulario de claims, y ninguno de los seis
problemas se plantea. Un gateway por realm es para cuando federar no es posible, o cuando el
segundo realm es de **otra clase** de sujeto — operadores en `master`, por ejemplo.

La opción 2 se rechaza por deriva: N artefactos que versionar, construir y escanear acaban
divergiendo. La 3, por los seis problemas y porque ninguno de ellos saca a nadie del camino
crítico: pone `auth-bff` y Valkey en serie detrás de un issuer más.

### El prerrequisito, cumplido el 2026-10-09

Hasta ese día esta decisión era inaplicable, y por dos líneas: `config/krakend.tmpl`
interpolaba `issuer` y `jwks_url` literalmente desde `jwt.json`. Eran **los dos únicos
valores del template que no leían su entorno**, mientras `docker-compose.yml` exportaba
`KEYCLOAK_ISSUER` y `KEYCLOAK_JWKS_URL` sin que nada los consumiera. Una imagen no podía
servir dos realms. Ahora los lee, `scripts/check-template-env.sh` vigila las dos direcciones,
y el prerrequisito está cerrado.

## Consequences

**Buenas.** Cinco de los seis problemas desaparecen sin tocar código. Cuota de rate limit por
realm sin configuración nueva, porque `service_max_rate` es por servicio. Usuario ACL de
Valkey por realm. El guardia de un solo issuer sigue en pie sin aflojar nada.

**Malas, o que cuestan.**

- N despliegues que operar, con DNS o un ingress por delante que enrute por host o por
  prefijo.
- **Aísla realms, no aplicaciones dentro de un realm.** Si dos apps son clientes del mismo
  realm —que es lo que ADR-0005 decide— las sirve el mismo gateway y un token de una abre las
  rutas de la otra. Eso es ADR-0006 y hace falta igual.
- Una ruta que deba aceptar tokens de dos realms se vuelve imposible. Es el precio de la
  separación, y conviene saberlo antes de necesitarla.

### Decisión abierta: qué subconjunto de rutas sirve cada despliegue

**Aplazada a propósito, con su condición de activación.**

Las rutas **se generan**: `endpoints.yaml` → `cmd/gen` → `config/settings/endpoints.json`, y
`make gen-check` falla si alguien edita el JSON a mano. Eso no cambia. Lo que está sin
decidir es el **subconjunto por despliegue**. `PRODUCTS` filtra —hoy `forgeos` 18,
`knowledge` 8, `platform` 5— pero lo hace en tiempo de `make gen`, y `endpoints.json` viaja
dentro de la imagen, así que un contenedor sirve siempre el conjunto con el que se construyó.

El riesgo concreto, cuando llegue: dos gateways desde la misma imagen exponen **las 31 rutas
los dos**, y lo único que los separa es el issuer. Un gateway de operadores expondría los 18
endpoints de ForgeOS protegidos por tokens de `master`, con lo que pertenecer al realm de
administración daría acceso a rutas de producto. Eso es peor que no tener el segundo gateway.

No se resuelve hoy porque depende de algo que todavía no se sabe: si los catálogos divergen.
Las tres salidas, para cuando haga falta:

1. Filtrar por `$e.product` dentro del `range` del template, con una variable de entorno.
   Mantiene una imagen. Coste real: ese bucle usa el índice para colocar las comas, así que
   saltarse entradas tiene su cuidado.
2. Una imagen por realm con `endpoints.json` filtrado — la opción 2 de arriba, con su deriva.
3. No filtrar, si todos los despliegues sirven el mismo catálogo y lo único que cambia es el
   issuer.

**Condición de activación:** la primera vez que exista un segundo despliegue que deba servir
un catálogo distinto del completo. Hasta entonces se configura a mano y no se construye
mecanismo. Mientras los operadores lleguen a la Admin API por `auth-bff` —lo que ADR-0005
decide—, ese segundo despliegue no serviría **ninguna** de estas 31 rutas, y no hay nada que
filtrar.

## Confirmation

Esta decisión no está implementada y no hace falta todavía: ADR-0005 establece
`codehunters` como el único realm de aplicaciones, así que hoy no hay un segundo realm que
desplegar. Lo que sí está es el prerrequisito, y tiene guardia:

- **`scripts/check-template-env.sh`**, en `make check`: falla si el template lee una variable
  que compose no pasa, o si compose pasa una que nada lee. Es lo que impide que `issuer` y
  `jwks_url` vuelvan a quedar fuera del entorno.
- **`scripts/check-jwt-single-issuer.sh`**, en `make check`: exige exactamente un `issuer` y
  un `jwks_url` del mismo realm. Un gateway por realm es precisamente lo que permite que ese
  guardia siga siendo correcto en vez de tener que aflojarlo.

Cuando exista el segundo despliegue, estas condiciones se añaden antes de pasar a `accepted`:

1. Un test que compruebe que cada despliegue rechaza con `401` un token del otro realm.
2. Una aserción de que cada despliegue tiene su propio usuario ACL de Valkey y su propio
   prefijo de clave.
3. La decisión de subconjunto de rutas, tomada y registrada.

## More Information

- Los seis problemas, medidos con fichero y línea, y por qué derivar el JWKS del `iss` es un
  bypass: esta misma conversación de diseño y el *Context* de arriba.
- Related: [ADR-0005](0005-one-platform-realm-and-master-as-operator-realm.md) — un realm de
  aplicaciones y `master` para operadores. Este ADR es la topología que hace seguro el día
  que el edge tenga que aceptar tokens de `master`.
- Related: [ADR-0003](0003-session-state-at-the-edge.md) — misma forma de argumento sobre la
  dirección de la dependencia, y la ACL de Valkey que esta topología aprovecha.
- Related: ADR-0006 (autorización por app en el edge, pendiente) — aísla aplicaciones dentro
  de un realm, que es lo que esta decisión **no** hace.
