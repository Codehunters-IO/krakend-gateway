---
status: proposed
date: 2026-10-05
decision-makers: [Carlos Andres Montoya Tobon]
consulted: [equipo plataforma, seguridad]
informed: [equipo frontend, equipo forgeos]
---

# ADR-0003: Estado de sesión en el edge — el gateway resuelve una cookie opaca contra Valkey

## Context and Problem Statement

El navegador no debe custodiar el JWT: un XSS se lo lleva y lo usa fuera del navegador. De ahí
el patrón Token Handler / BFF, diseñado en
[`2026-08-31-token-handler-bff-design.md`](../superpowers/specs/2026-08-31-token-handler-bff-design.md)
(bajo `docs/superpowers/specs/`) e implementado en `plugins/session-resolver/` más el servicio
`codehunters/auth-bff`.

El patrón exige que alguien traduzca cookie → token en cada petición. La guía de plataforma
asigna la autenticación de borde al gateway («API Gateway: TLS termination, **AuthN edge**, …»)
y a la vez pide que el gateway sea stateless: «No session, no DB lookups in plugins».

Esa regla apunta a **estado por petición**: sesiones en memoria del proceso, consultas de datos
de negocio, afinidad de instancia. No apunta a dependencias externas en el camino de
autenticación, porque la propia guía las prescribe: `jwt.json` lleva `jwks_url` y
`cache_ttl_minutes: 60`, y pide fallar cerrado si la caché está vacía y el fetch falla. El edge
ya devuelve `503` en toda ruta protegida cuando Keycloak no responde
(`plugins/jwt-headers/main.go:226`).

Lo que esta decisión añade sobre eso son dos cosas concretas, y son las que hay que justificar:

1. **Frecuencia.** El JWKS se consulta una vez por TTL de 60 minutos. La sesión se consulta una
   vez por petición con cookie.
2. **Naturaleza de la dependencia.** El JWKS es un endpoint público de solo lectura del IdP.
   Valkey es un almacén con los access tokens de los usuarios vivos dentro.

El spec tomó nueve decisiones finas y bien argumentadas —transporte del id, valor opaco frente
al claim `sid`, dueño del flujo OIDC, lenguaje de `auth-bff`, modo dual, caché, persistencia,
colocación de componentes— pero no planteó estas dos. Este ADR las plantea.

## Decision Drivers

- **El JWT no llega al navegador.** Driver dominante; es la razón de existir del patrón.
- **Revocación inmediata, sin ventana de caché.** Es lo que hace útil el backchannel logout.
- **Clientes no-navegador sin cambios**: MCP sobre Streamable HTTP, CI, Postman, móvil.
- **El edge no custodia `client_secret` ni refresh tokens.**
- **`auth-bff` fuera del camino de la petición.** El gateway resuelve la sesión por sí mismo,
  contra Valkey, sin consultar a `auth-bff`. En régimen estacionario `auth-bff` no participa:
  solo entra en el refresh perezoso, cuando al access token le quedan menos de 30 s, y solo lo
  llama el ganador de un lock de 5 s (`plugins/session-resolver/refresh.go:303`). Con `exp` de
  ~5 min eso es del orden de una llamada cada 4,5 min por sesión activa, no una por petición.
- **Escalabilidad horizontal del gateway**: cualquier instancia sirve cualquier petición.
- **Coste por petición acotado y predecible.**

## Considered Options

1. **Lookup en el plugin del edge contra Valkey.**
2. **El plugin llama a `auth-bff` por petición.**
3. **`auth-bff` como proxy inverso delante del gateway**: termina la cookie, inyecta el
   `Bearer`, y el gateway queda sin dependencia de sesión.
4. **Cookie que transporta el token sellado o cifrado**: sin almacén, stateless de verdad.
5. **El SPA custodia el token**: el estado anterior, el que el patrón viene a eliminar.

## Decision Outcome

Opción elegida: **"Lookup en el plugin del edge contra Valkey"** (opción 1).

La regla stateless se conserva en la propiedad que protege para escalar —el plugin no guarda
estado local, cualquier instancia sirve cualquier petición, no hay sticky sessions— y se cede en
la otra: Valkey entra como dominio de fallo compartido para el tráfico con cookie, con fallo
cerrado en `503`. El gateway queda stateless como proceso y dependiente como sistema, igual que
ya lo era respecto al JWKS, con las dos diferencias de frecuencia y naturaleza dichas arriba.

Tres propiedades del diseño acotan el coste de esa cesión, y son las que hacen aceptable la
opción:

- **Una lectura, cuatro campos.** El plugin emite un único
  `HMGET access_token exp abs_exp sub` contra un almacén en memoria, con timeout de 200 ms. No
  lee el hash completo ni hace varias idas y venidas.
- **Read-mostly, no una escritura por petición.** El TTL de inactividad se renueva solo cuando
  queda menos de la mitad; con los valores provisionales de hoy (`TTL < 15min → EXPIRE 30min`),
  del orden de una escritura cada 15 minutos por sesión activa en vez de una por petición.
- **El edge no puede acuñar tokens.** `refresh_token_enc` e `id_token_enc` están cifrados con
  AES-256-GCM y la clave vive solo en `auth-bff`. Un edge comprometido entrega access tokens de
  vida corta, no la capacidad de renovarlos indefinidamente.

**Sobre las opciones 2 y 3.** Las dos caen por el mismo driver: `auth-bff` no debe estar en el
camino de la petición. El spec rechaza la 2 por el salto JVM en cada request y por convertir a
`auth-bff` en dependencia dura de todo el tráfico. La 3 es una topología distinta —`auth-bff`
delante, el gateway detrás sin saber de sesiones— y viola ese driver **más** que la 2, no menos:
la 2 mete el salto solo en el tráfico con cookie, mientras la 3 lo mete en todo, incluido el que
llega con `Authorization` y hoy no paga nada. A cambio, la 3 es la única que preserva la regla
stateless en su lectura literal, y obligaría a mover TLS, CORS, rate limit y propagación de
trace context al nuevo borde, que hoy están resueltos en el gateway.

Que el gateway acceda a Valkey por su cuenta **es el punto del diseño**, no un atajo: es lo que
mantiene a `auth-bff` fuera del régimen estacionario y hace que una caída suya no tumbe el
tráfico ya autenticado.

**Sobre la opción 4.** Rechazada. Es la única alternativa realmente stateless, y el trade que
cambia es justo el driver que más pesa: una cookie que transporta el token sellado no se puede
revocar antes de su expiración, así que el backchannel logout deja de tener efecto inmediato.

## Consequences

- **Good** — El JWT nunca llega al navegador; `HttpOnly` impide extraer la credencial.
- **Good** — Revocación inmediata: sin caché en el plugin, borrar la clave corta el acceso.
- **Good** — El tráfico con `Authorization` entrante no toca Valkey: coste cero y superficie
  intacta para MCP, CI y móvil.
- **Good** — `client_secret` y refresh tokens se quedan en `auth-bff`, fuera del edge.
- **Bad** — Valkey es dominio de fallo compartido para el tráfico con cookie. Cae el almacén,
  caen las rutas protegidas del navegador con `503`.
- **Bad** — 200 ms de timeout en el camino de la petición por cada request con cookie.
- **Bad** — `auth-bff` queda fuera del régimen estacionario pero no del todo: si está caído
  cuando toca refrescar, la petición devuelve `503`, porque el token viejo puede estar ya
  revocado. El radio es una petición cada ~4,5 min por sesión activa, no todas.
- **Bad** — Un edge comprometido ve access tokens en claro. Inevitable: el plugin debe
  inyectarlos. Acotado por su vida corta y por el cifrado del refresh.
- **Bad** — Desvía de la lectura literal de la guía de plataforma, lo que obliga a este ADR.
  Quien lea la guía y luego el plugin encontrará la misma contradicción aparente; este documento
  es dónde se resuelve.
- **Neutral** — Un reinicio de Valkey desloguea a todos; es transparente mientras viva la sesión
  SSO de Keycloak.
- **Neutral** — Tres relojes independientes que hay que mantener alineados: `exp` como campo que
  dispara el refresh, el TTL de inactividad como TTL real de la clave, y `abs_exp` como techo
  duro que no se extiende. Los valores que maneja el diseño —~5 min, 30 min y 10 h— son
  **provisionales**: el spec los deja como *Open Item* a la espera de leer *Access Token
  Lifespan* y *SSO Session Max* del realm en Keycloak. `abs_exp` debe cuadrar con *SSO Session
  Max*, o un techo más corto corta sesiones vivas y uno más largo deja de ser techo. Fijar esos
  valores es requisito para aceptar este ADR, igual que el job de CI.

**Interacción con los realms, resuelta por ADR-0005.** Este ADR se escribió cuando ADR-0002 elegía
realm por producto, y dejaba abierto qué ocurre con una sola cookie de sesión frente a varios
realms: un usuario con sesión de un realm pegando a la ruta de otro producto debía recibir `401`
por la comprobación de `iss`, pero eso era una consecuencia emergente de dos diseños
independientes, no una decisión tomada.

[ADR-0005](0005-one-platform-realm-and-master-as-operator-realm.md) cierra la pregunta por
construcción: `codehunters` es el único realm de aplicaciones y cada aplicación es un cliente
dentro de él, de modo que una cookie de sesión y un issuer se corresponden uno a uno. No hay
sesión cruzada entre realms de producto porque no hay varios realms de producto.

Queda una variante más estrecha, y conviene no perderla de vista: ADR-0005 hace de `master` el
realm de operadores. Mientras los operadores lleguen a la Admin API a través de `auth-bff` con su
propio token y no por una ruta protegida de este gateway, el edge sigue viendo un solo issuer y
este diseño no cambia. **Si algún día el gateway tiene que aceptar además tokens de `master`, la
pregunta vuelve**: una cookie de sesión tendría que decir a qué realm pertenece, y el contrato
`v1:session:{sid}` no lleva ese campo hoy. Añadirlo es barato; descubrirlo tarde, no.

## Confirmation

**Cubierto hoy.** 32 tests en `plugins/session-resolver/` sobre las propiedades que importan:

- Fallo cerrado con el almacén caído (`TestHandlerFailsClosedWhenValkeyIsDown`) y ante acción de
  valor cero (`TestZeroValueActionFailsClosed`, `TestActionResponseDeniesUnknownAction`).
- El `sid` nunca se loguea ni se filtra por error de URL
  (`TestHandlerRefreshFailureNeverLogsTheSid`, `TestCallAuthBffNeverLeaksSidViaURLError`).
- La clase de error no refleja el texto del mensaje
  (`TestRefreshErrorClassNeverReflectsMessageText`).
- Techo `abs_exp` (`TestRefreshAbsExpCeiling`), token almacenado vacío denegado, `sid` malformado
  denegado sin tocar el almacén, y carga de solo campos no secretos
  (`TestStoreLoadRequestsOnlyNonSecretFields`).

**No cubierto, y hay que decirlo.** Esos 32 tests **no corren en ningún job de CI**. El pipeline
construye la imagen —lo que valida que los plugins compilan— y audita la configuración de
KrakenD, pero no ejecuta `go test` de los plugins. La confirmación de este ADR está escrita y no
aplicada hasta que exista ese job. Es exactamente la deriva que ADR-0001 registró honestamente
al aceptarse, y nombrarla aquí es lo que evita repetirla: **este ADR no debe pasar a `accepted`
sin ese job en verde.** Concretamente, el job debe correr `go test ./...` sobre
`plugins/session-resolver/` en cada pull request, y fallar el check si algún test falla. Mientras
no exista, los 32 tests son documentación ejecutable a mano, no una confirmación.

**Pendiente además:**

- Métrica de ratio de `503` por clase de error del almacén; una deriva es alerta.
- Un smoke test que compruebe lo que sale por el socket, no solo el render: cookie válida → `200`
  con `Authorization` inyectada; almacén caído → `503`; `Bearer` entrante → pasa intacto.

**Consecuencia de runbook.** El tráfico con `Bearer` sobrevive a una caída de Valkey y el del
navegador no. MCP y CI siguen vivos mientras el front está caído.

### Condiciones para pasar a `accepted`

1. Un job de CI que corra `go test ./...` sobre `plugins/session-resolver/` en cada pull request
   y falle el check si algún test falla.
2. Valores definitivos de los tres relojes, leídos de *Access Token Lifespan* y *SSO Session Max*
   del realm, sustituyendo los provisionales.

Lo demás de esta sección —métrica de `503` por clase y smoke test sobre el socket— es deuda
reconocida, no bloqueo.

## Pros and Cons of the Options

### 1. Lookup en el plugin contra Valkey (elegida)
- Bueno: una lectura de cuatro campos en memoria; revocación inmediata; sin salto JVM.
- Bueno: el `Bearer` entrante no paga nada.
- Malo: Valkey en el camino de la petición y como dominio de fallo compartido.

### 2. El plugin llama a `auth-bff` por petición
- Bueno: el edge no conoce el contrato de datos.
- Malo: salto JVM en cada request y `auth-bff` como dependencia dura de todo el tráfico, cuando
  un contrato de clave versionado da el mismo desacoplamiento.

### 3. `auth-bff` como proxy inverso delante del gateway
- Bueno: el gateway queda stateless en la lectura literal de la guía; la sesión no sale de
  `auth-bff`.
- Malo: un salto más en **todo** el tráfico, incluido el que llega con `Authorization` y hoy no
  paga nada; `auth-bff` pasa a ser el borde real y hereda TLS, CORS, rate limit y propagación de
  trace context, resueltos hoy en el gateway; y su caída tumba también el tráfico ya autenticado.

### 4. Cookie con el token sellado
- Bueno: sin almacén; stateless de verdad.
- Malo: sin revocación inmediata, que es el driver dominante; tamaño de cookie.

### 5. El SPA custodia el token
- Bueno: nada que construir.
- Malo: un XSS se lleva la credencial fuera del navegador. Es el problema de partida.

## More Information

- Diseño y mecánica completa (flujos de login, petición autenticada, refresh perezoso y logout;
  contrato `v1:session:{sid}`; CSRF; rotación): el spec enlazado arriba.
- Flujo en ejecución, orden de la cadena de plugins y runbook:
  [`docs/session-flow.md`](../session-flow.md).
- Related: [ADR-0005](0005-one-platform-realm-and-master-as-operator-realm.md) (un realm de
  aplicaciones, `master` para operadores), que sustituye a ADR-0002 y resuelve la pregunta de la
  sesión frente a varios realms — ver *Consequences*.
- Related: [ADR-0002](0002-platform-edge-global-keycloak-realm-per-product.md), superseded. Su
  mitad «edge de plataforma compartido» sigue vigente y es la que sostiene este diseño; la mitad
  «realm por producto» no.
