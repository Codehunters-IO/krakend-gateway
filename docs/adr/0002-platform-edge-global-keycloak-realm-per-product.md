---
status: proposed
date: 2026-08-02
decision-makers: [Carlos Andres Montoya Tobon]
consulted: [equipo plataforma, seguridad]
informed: [equipo frontend, equipo forgeos]
---

# ADR-0002: Edge de plataforma compartido + Keycloak global con realm-por-producto (validación multi-realm por prefijo en el edge)

## Context and Problem Statement

El gateway KrakenD (genericizado desde el gateway interno de Codehunters, ver ADR-0001)
está pensado como **base de plataforma**: un único edge que fronta múltiples productos
(ForgeOS y otros microservicios/proyectos del ecosistema). Hoy la realidad es la contraria:
corren **dos** instancias de Keycloak en paralelo (`keycloak-keycloak-1` :8082 genérico y
`forgeos-keycloak-1` :8083 específico de ForgeOS), cada producto valida su propio JWT contra
su propio IdP, y no hay edge común — el frontend de ForgeOS pega directo al backend `:8080`.

Se busca consolidar: **un** Keycloak global como IdP de todos los productos y **un** gateway
compartido como edge. La pregunta arquitectónica es cómo modelar la identidad (¿realms
separados por producto o un realm compartido?) y cómo un único gateway valida tokens de
productos distintos, sin que el edge se convierta en cuello de botella ni en punto de
acoplamiento de identidad entre productos.

## Decision Drivers

- **Aislamiento entre productos**: pool de usuarios, roles y client scopes separado por producto.
- **Un solo IdP operativo**: una fuente de JWKS por realm; sin duplicar infra (hoy 2 KC).
- **Edge único y consistente**: validación JWT + claims→headers + CORS + rate-limit + W3C trace
  aplicados igual para todos los productos, sin código por servicio.
- **Reutilizar lo existente**: el plugin Go `jwt-headers` ya hace validación single-realm +
  claims→headers; minimizar cambio.
- **Rollout reversible e incremental**: sin big-bang; cada paso shippable y revertible.
- **Multi-tenancy anidada**: ForgeOS es multi-tenant por dentro (`organizationId`), así que el
  modelo debe soportar `IdP → realm producto → orgs`.

## Considered Options

1. **Realm-por-producto + edge valida por prefijo de ruta** (extender `jwt-headers` a `realms[]`).
2. **Realm compartido + client-por-producto** (un único `issuer`/JWKS para todo el edge).
3. **Híbrido**: realm compartido para productos con SSO común, realm aparte para los aislados.
4. **Una instancia de gateway por producto** (cada KrakenD con su `jwt.json` single-realm).
5. **Keycloak fronteado por el gateway en `/auth/*`** (dominio público único).
6. **Status quo**: un Keycloak por producto, sin edge común.

## Decision Outcome

Opción elegida: **"Realm-por-producto + edge valida por prefijo de ruta"** (opción 1), porque
maximiza el aislamiento entre productos (driver dominante) manteniendo **un** IdP y **un** edge,
y encaja con la multi-tenancy anidada de ForgeOS (`realm forgeos → orgs`).

Decisiones acompañantes que forman parte de este outcome:

- **El gateway fronta solo APIs; Keycloak permanece en su host propio** (se rechaza la opción 5
  como default). El SPA hace el baile OIDC (login/token/JWKS) **directo** contra Keycloak y
  envía el bearer al gateway. Fronterar KC tras el edge obliga a `KC_HOSTNAME` + `X-Forwarded-*`
  para que el `issuer` emitido coincida con la URL pública y complica el redirect del login;
  solo se justifica si se exige un único dominio público, requisito que hoy no existe.
- **La validación multi-realm vive en el plugin `jwt-headers`** vía una tabla `realms[]` con
  match de **longest-prefix**: cada entrada `{ prefix, issuer, jwks_url, claims_to_headers }`.
  El plugin selecciona el realm por el prefijo de ruta, valida contra ese JWKS e inyecta los
  headers de ese realm. Se rechaza la opción 4 (instancia por producto) por multiplicar la
  operación (N despliegues, N configs) sin ganar aislamiento real de identidad.
- **Rollout incremental 1→2→3**, cada paso aislado del trabajo de producto en curso:
  1. **Edge ForgeOS single-realm** — gateway `:8090` fronta `/api/*` → backend `:8080`, realm
     `forgeos`. El plugin ya hace single-realm → sin tocar Go. Prueba el edge end-to-end.
  2. **Plugin multi-realm** — extender `jwt-headers` a `realms[]` por prefijo + tests; sumar
     `codehunters` y otros productos.
  3. **Keycloak global** — consolidar los 2 KC en uno; migrar el realm `forgeos` tal cual
     (mismo realm, host nuevo); actualizar `issuer-uri` (backend forgeos) y
     `VITE_KEYCLOAK_AUTHORITY` (frontend); apagar `forgeos-keycloak-1`.

### Consequences

- **Good** — Aislamiento fuerte por producto (usuarios/roles/scopes separados) con un solo IdP.
- **Good** — Un edge cubre validación JWT, claims→headers, CORS, rate-limit y tracing para todos.
- **Good** — Reversible e incremental: el Paso 1 aporta valor sin tocar Go ni migrar KC.
- **Good** — El realm `forgeos` migra sin reescritura (mismo export, host distinto).
- **Bad** — El plugin `jwt-headers` debe extenderse a multi-realm (trabajo Go + tests); hasta el
  Paso 2 el edge es single-realm y solo sirve a ForgeOS.
- **Bad** — Un Keycloak global es dominio de fallo compartido: KC caído = todos los productos
  caídos → exige HA de KC en producción (fuera del scope de este ADR, pero condiciona el Paso 3).
- **Bad** — La migración del realm `forgeos` es un cambio coordinado cross-repo (issuer backend +
  authority frontend) → requiere ventana y nota/ADR en el repo `forgeos`.
- **Neutral** — Dos hostnames públicos (KC + gateway) en vez de uno; aceptable salvo requisito de
  dominio único, que reabriría la opción 5 en un nuevo ADR.

### Confirmation

- **Test de routing del plugin** (`plugins/jwt-headers/matchers_test.go`): un token del realm
  `forgeos` valida en `/api/*` e inyecta `X-Organization-Id`; el mismo token contra el prefijo de
  otro producto → 401; sin token en ruta protegida → 401; `skip_paths` (p.ej. `/api/ping`) → pasa.
- **Smoke en CI** (`pull-request.yml`): `curl` con token `forgeos` a `/api/projects` → 200; token
  de realm equivocado → 401; `/api/ping` sin token → 200.
- **`krakend check -d -t -c krakend.json`**: valida render del template + schema con `realms[]`.
- **Métrica runtime**: contador de rechazos JWT etiquetado por realm/prefijo (deriva = alerta).

## Pros and Cons of the Options

### 1. Realm-por-producto + edge por prefijo (elegido)
- Bueno: aislamiento máximo de identidad; un IdP; un edge; encaja con orgs de ForgeOS.
- Bueno: reutiliza `jwt-headers`; extensión acotada (`realms[]` + longest-prefix).
- Malo: requiere trabajo Go para multi-realm; edge single-realm hasta el Paso 2.

### 2. Realm compartido + client-por-producto
- Bueno: **un** `issuer`/JWKS → gateway trivial (sin extender el plugin) + SSO entre productos.
- Malo: todos los productos comparten pool de usuarios → sin aislamiento; un scope/rol mal puesto
  cruza fronteras de producto.

### 3. Híbrido
- Bueno: SSO donde conviene, aislamiento donde se exige.
- Malo: dos modelos mentales conviviendo; config del edge mixta y más difícil de razonar; se puede
  llegar aquí después si un producto concreto pide SSO — no es el default.

### 4. Instancia de gateway por producto
- Bueno: cada gateway con `jwt.json` single-realm, sin extender el plugin.
- Malo: N despliegues/configs a operar; duplica todo el edge; no aporta aislamiento de identidad
  que los realms no den ya.

### 5. Keycloak fronteado por el gateway (`/auth/*`)
- Bueno: dominio público único para todo.
- Malo: obliga `KC_HOSTNAME`/`X-Forwarded-*` para alinear `issuer`; complica redirect de login;
  acopla el ciclo OIDC al edge. Solo si hay requisito de dominio único → nuevo ADR.

### 6. Status quo (KC por producto, sin edge)
- Bueno: nada que migrar.
- Malo: infra duplicada (2 KC hoy); sin cross-cutting común; no escala a plataforma.

## More Information

- Diseño y secuencia detallada (topología `:8090`, mapeo `organizationId`→`X-Organization-Id`,
  manejo de SSE, strip de headers de identidad entrantes): sesión de diseño 2026-08-02.
- El backend de ForgeOS lee el claim `organizationId` (`JwtCurrentUser`) y acepta el header
  `X-Organization-Id` con re-check de membresía (D-12) → el edge lo inyecta desde el claim
  validado y **debe** stripear cualquier `X-Organization-Id`/`x-user-*` entrante del cliente.
- Related: ADR-0001 (cabeceras de seguridad en el edge). Migración del `issuer` del realm forgeos
  → requiere nota/ADR en el repo `forgeos`.
- Patrón settings + env-override: `README.md`, `config/settings/`.
