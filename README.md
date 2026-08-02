# KrakenD API Gateway

API Gateway **genérico y reusable** construido con [KrakenD](https://www.krakend.io/) `2.13.4`. Sirve de base para cualquier plataforma: clona, define tus endpoints y apunta a tus backends.

Aporta out-of-the-box los cross-cutting concerns de un gateway de producción — **JWT/Keycloak**, trazabilidad W3C, geo-IP, i18n, timeouts, rate-limit, CORS, TLS y cabeceras de seguridad — via **5 plugins Go** custom + módulos nativos. El bloque `endpoints` trae 2 ejemplos de referencia (uno protegido, uno público) que reemplazas por los tuyos.

## Estructura del Proyecto

```
krakend-gateway/
├── config/
│   ├── krakend.tmpl                  # Template principal (Flexible Configuration)
│   └── settings/
│       ├── service.json              # Nombre, puerto, timeouts
│       ├── hosts.json                # Hosts de los servicios backend
│       ├── cors.json                 # Configuracion CORS
│       ├── jwt.json                  # JWT/JWKS (Keycloak)
│       ├── ip_resolver.json          # Geolocalizacion de IP
│       ├── trace_context.json        # W3C Trace Context
│       ├── accept_language.json      # i18n: Accept-Language por defecto
│       ├── gateway_timeout.json      # Rewrite 500 → 504 en timeouts de backend
│       ├── rate_limit.json           # Rate limiting
│       ├── logging.json              # Logging
│       ├── metrics.json              # Metricas y telemetria
│       ├── security_headers.json     # Cabeceras de seguridad edge (HSTS, X-Frame, nosniff)
│       ├── tls.json                  # TLS server-side (HTTPS del gateway)
│       └── client_tls.json           # TLS cliente hacia backends HTTPS
├── plugins/
│   ├── jwt-headers/                  # Plugin: validacion JWT + extraccion de claims
│   ├── ip-resolver/                  # Plugin: geolocalizacion de IP via ip-api.com
│   ├── trace-context/                # Plugin: propagacion W3C Trace Context
│   ├── accept-language/              # Plugin: Accept-Language por defecto (i18n)
│   ├── gateway-timeout/              # Plugin: reescribe 500 → 504 en timeouts
│   ├── Dockerfile.builder            # Imagen para compilar los plugins (Linux .so)
│   └── build/                        # Plugins compilados (.so)
├── docs/
│   └── adr/                          # Architectural Decision Records (MADR)
├── Dockerfile                        # Build multi-stage (produccion)
├── docker-compose.yml                # Entorno de desarrollo local
└── Makefile                          # Comandos de build y desarrollo
```

Decisiones arquitectonicas: ver [`docs/adr/`](docs/adr/README.md).

## Quick Start

### Desarrollo local

Compila los plugins y levanta el gateway:

```bash
make dev
```

Esto ejecuta dos pasos:
1. `plugin-build` - Compila los 5 plugins dentro de Docker (compatibles con Linux)
2. `up` - Levanta KrakenD con `docker-compose`, montando `config/` y `plugins/build/` como volumenes

Si solo cambiaste configuracion (sin tocar codigo de plugins):

```bash
docker compose restart
```

### Apuntar el gateway a tus backends

Los hosts definidos en `config/settings/hosts.json` son **defaults**. Cada uno puede sobreescribirse via variable de entorno (desde el shell o un `.env` junto al `docker-compose.yml`). El template resuelve `{{ env "EXAMPLE_HOST" | default .hosts.example }}` — la envvar gana si está seteada; si no, usa `hosts.json`.

El scaffold trae un único backend de ejemplo:

| Envvar | Default (`hosts.json`) | Apunta a |
|--------|------------------------|----------|
| `EXAMPLE_HOST` | `http://example-service:8080` | Backend de ejemplo (endpoints `/api/v1/example`, `/public/v1/health`) |

**Añadir tus propios backends:** por cada servicio, agrega una clave en `hosts.json` (`"auth": "http://auth:8080"`) y referénciala en el endpoint con `{{ env "AUTH_HOST" | default .hosts.auth }}`. Declara `AUTH_HOST` en `docker-compose.yml` para poder overridearlo por entorno.

#### Caso 1 — Backend corriendo en host local (fuera de Docker)

Usar `host.docker.internal` para que KrakenD dentro del contenedor llegue al puerto del host:

```bash
# .env junto a docker-compose.yml
EXAMPLE_HOST=http://host.docker.internal:9095
```

```bash
make down && make up
```

#### Caso 2 — Backend en otro container con docker-compose propio

Conectar el gateway a la red del backend y apuntar al nombre del servicio:

```bash
# .env
EXAMPLE_HOST=http://my-service:9095

# unirse a la red externa donde vive el backend
docker network connect my-service_default krakend-gateway-krakend-1
```

#### Caso 3 — Mix: gateway local + backends remotos

```bash
EXAMPLE_HOST=https://api.dev.example.com \
make up
```

#### Verificar overrides activos

```bash
docker compose config | rg HOST          # ve los valores resueltos
docker compose exec krakend env | rg HOST
make generate && rg '"host"' krakend.json | sort -u
```

#### Override sin contenedor (KrakenD nativo via `make run`)

```bash
EXAMPLE_HOST=http://localhost:9095 \
make run
```

### Produccion

Build completo con imagen Docker multi-stage:

```bash
make build
```

## Comandos disponibles

| Comando | Descripcion |
|---------|-------------|
| `make dev` | Compila plugins + levanta docker-compose (desarrollo) |
| `make up` | Levanta docker-compose (plugins ya compilados) |
| `make down` | Detiene docker-compose |
| `make logs` | Muestra logs del gateway |
| `make build` | Construye imagen Docker de produccion |
| `make plugin-build` | Compila todos los plugins con Docker |
| `make plugin-check` | Verifica que plugins + config son validos |
| `make check` | Valida la configuracion KrakenD |
| `make generate` | Genera el `krakend.json` final desde templates |
| `make clean` | Elimina artefactos generados |

## Plugins

El gateway utiliza 5 plugins custom registrados como HTTP server middleware (`plugin/http-server`). Se ejecutan en cadena **antes** del routing de endpoints, en este orden:

| # | Plugin | Descripcion | Settings |
|---|--------|-------------|----------|
| 1 | **gateway-timeout** | Reescribe respuestas `500` de backend a `504 Gateway Timeout` cuando el tiempo transcurrido supera `min_elapsed` (KrakenD Community devuelve 500 en timeout, no 504) | `gateway_timeout.json` |
| 2 | **accept-language** | Fija `Accept-Language` por defecto (`es`) cuando el cliente no lo envia | `accept_language.json` |
| 3 | **trace-context** | Propaga headers W3C Traceparent/Tracestate. Genera trace IDs si no existen | `trace_context.json` |
| 4 | **ip-resolver** | Resuelve IP del cliente a geolocalizacion (pais, ciudad, coordenadas) via ip-api.com con cache | `ip_resolver.json` |
| 5 | **jwt-headers** | Valida JWT contra JWKS de Keycloak y mapea claims a headers HTTP (x-username, x-user-roles, x-user-id) | `jwt.json` |

El orden lo determina el template `krakend.tmpl`: cada plugin se añade a la lista `plugin/http-server.name` solo si su flag `enabled` esta activo.

### Habilitar / Deshabilitar plugins

Cada plugin tiene un campo `enabled` en su fichero de settings:

```json
// config/settings/trace_context.json
{ "enabled": true }

// config/settings/ip_resolver.json
{ "enabled": true, ... }

// config/settings/jwt.json
{ "enabled": true, ... }

// config/settings/accept_language.json
{ "enabled": true, "default_value": "es" }

// config/settings/gateway_timeout.json
{ "enabled": true, "trigger_status": 500, "timeout_status": 504, "min_elapsed": "4900ms" }
```

Cambia `"enabled"` a `true` o `false` y reinicia el gateway.

Estado actual de flags: `gateway-timeout` ✅ · `accept-language` ✅ · `trace-context` ✅ · `ip-resolver` ✅ · `jwt-headers` ✅ (todos activos).

### Headers inyectados / modificados por plugins

| Header | Plugin | Descripcion |
|--------|--------|-------------|
| `Accept-Language` | accept-language | Se fija a `es` si el request no lo trae |
| `Traceparent` | trace-context | ID de traza W3C (front + backend) |
| `Tracestate` | trace-context | Estado de traza W3C |
| `X-Traceparent` | trace-context | Copia del Traceparent con prefijo `x-`, solo backend |
| `x-geo-country` | ip-resolver | Pais del cliente |
| `x-geo-city` | ip-resolver | Ciudad del cliente |
| `x-geo-latitude` | ip-resolver | Latitud |
| `x-geo-longitude` | ip-resolver | Longitud |
| `x-geo-ip` | ip-resolver | IP publica resuelta |
| `x-username` | jwt-headers | Username del token JWT |
| `x-user-roles` | jwt-headers | Roles del usuario |
| `x-user-id` | jwt-headers | Subject (ID) del usuario |
| `x-ip` | jwt-headers | IP del cliente |

> `gateway-timeout` no inyecta headers: solo reescribe el status code de la respuesta (`500` → `504`) cuando el backend excede el timeout.

## Trazabilidad (W3C Trace Context)

El gateway propaga trazas distribuidas siguiendo el estandar [W3C Trace Context](https://www.w3.org/TR/trace-context/) mediante el plugin `trace-context`.

### Headers

| Header | Direccion | Descripcion |
|--------|-----------|-------------|
| `Traceparent` | Front → Gateway | Identificador W3C completo de la traza |
| `Tracestate` | Front → Gateway | Estado adicional especifico del vendor (opcional) |
| `Trace-Id` | Front → Gateway | Fallback: solo el `trace-id` de 32 hex si el front no construye `Traceparent` completo |
| `X-Traceparent` | Gateway → Backend | Copia del `Traceparent` con prefijo `x-` (convencion interna) |

Formato de `Traceparent`:

```
00-<trace-id>-<parent-id>-<flags>
```

| Campo | Tamaño | Ejemplo |
|-------|--------|---------|
| `version` | 2 hex | `00` |
| `trace-id` | 32 hex | `4bf92f3577b34da6a3ce929d0e0e4736` |
| `parent-id` | 16 hex | `00f067aa0ba902b7` |
| `flags` | 2 hex | `01` (sampled) o `00` (no sampled) |

Ejemplo completo:

```
traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
```

### Comportamiento del gateway

Orden de resolucion (primer match gana):

1. **Cliente envia `Traceparent` con formato W3C valido** → se propaga sin modificar.
2. **Cliente NO envia `Traceparent` (o invalido) PERO envia `Trace-Id` con 32 hex** → gateway construye `Traceparent` usando ese `trace-id` + `parent-id` aleatorio + `flags=01`.
3. **Ninguno valido** → gateway genera todo (`trace-id` + `parent-id` aleatorios).

Despues de resolver:

4. El gateway añade `X-Traceparent` (mismo valor que `Traceparent`) siguiendo la convencion `x-` de los backends.
5. Ambos headers (`Traceparent` + `X-Traceparent`) se reenvian a los servicios backend via `input_headers` en todos los endpoints.
6. CORS expone `Traceparent`, `Tracestate` y `Trace-Id` en `expose_headers` para que clientes web puedan leerlos en respuestas.

> CORS actual: `allow_headers: ["*"]` — el gateway acepta cualquier header del front. Esto incluye `Traceparent` y `Trace-Id` sin necesidad de listarlos. La proteccion contra spoofing de identidad NO depende de CORS sino del strip que debe hacer el plugin `jwt-headers` (ver seccion "Seguridad").

### Convencion: front vs backend

| Audiencia | Header | Razon |
|-----------|--------|-------|
| Front / clientes externos | `Traceparent` (W3C) | Estandar interoperable. OTEL/Micrometer Tracing lo entiende nativo |
| Backends internos | `X-Traceparent` + `Traceparent` | Backends esperan convencion `x-` para headers internos. `Traceparent` mantiene propagacion OTEL nativa |

Flujo:

```
Cliente → Gateway:
  Traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01

Gateway → Backend:
  Traceparent:   00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01   (W3C/OTEL)
  X-Traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01   (convencion interna)
```

### Funcionamiento interno del plugin

Implementacion: `plugins/trace-context/main.go`. Plugin Go compilado como `trace-context.so` y registrado en KrakenD via `plugin/http-server`. Se ejecuta como middleware HTTP **antes** del routing de endpoints — todos los requests pasan por aqui.

#### Pseudo-codigo

```go
HandlerFunc(req):
    traceparent = req.Header["Traceparent"]

    // 1. Validar formato W3C
    if NOT match(traceparent, /^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$/):

        // 2. Fallback: leer Trace-Id del front
        traceID = req.Header["Trace-Id"]
        if NOT match(traceID, /^[0-9a-f]{32}$/):
            traceID = randomHex(16)            // 32 hex chars

        // 3. Generar parent-id (siempre random, representa el span del gateway)
        parentID = randomHex(8)                // 16 hex chars

        // 4. Construir Traceparent W3C
        traceparent = "00-" + traceID + "-" + parentID + "-01"
        req.Header["Traceparent"] = traceparent

    // 5. Inyectar copia con prefijo x- para backends
    req.Header["X-Traceparent"] = traceparent

    next(req)
```

#### Tabla de decision

| `Traceparent` entrante | `Trace-Id` entrante | `trace-id` final | `parent-id` final | Origen |
|------------------------|---------------------|------------------|-------------------|--------|
| W3C valido | * (ignorado) | el del front | el del front | propagado |
| invalido / ausente | 32 hex valido | el `Trace-Id` del front | random gateway | reconstruido |
| invalido / ausente | invalido / ausente | random gateway | random gateway | generado |

#### Detalles tecnicos

| Aspecto | Valor |
|---------|-------|
| Regex `Traceparent` | `^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$` |
| Regex `Trace-Id` fallback | `^[0-9a-f]{32}$` |
| Generador random | `crypto/rand` (Go) → fallback a ceros si falla |
| Version W3C usada | `00` (actual) |
| Flags por defecto | `01` (sampled) |
| `parent-id` | Siempre random — representa el span del gateway, NO se preserva del cliente |

#### Headers que toca el plugin

| Header | Lectura | Escritura |
|--------|---------|-----------|
| `Traceparent` | Si | Si (solo si invalido/ausente) |
| `Trace-Id` | Si (solo fallback) | No |
| `X-Traceparent` | No | **Siempre** (sobrescribe si el cliente lo envio) |

#### Lo que el plugin **NO** hace

- No valida `Tracestate` (se reenvia tal cual via `input_headers`).
- No persiste ni emite spans propios — es solo propagacion. Para tracing real, los backends deben tener OTEL/Micrometer.
- No filtra ni decide muestreo — el flag `01` es estatico.
- No re-genera `parent-id` del cliente — si el front envia un `Traceparent` valido, ese `parent-id` se preserva (no se sobrescribe).

#### Performance

Sin alocaciones costosas: dos `regexp.MatchString` (precompilados a nivel package), uno o dos `rand.Read` de pocos bytes, un `Sprintf`. Coste por request: microsegundos. Sin I/O, sin dependencias externas.

### Habilitar / Deshabilitar

`config/settings/trace_context.json`:

```json
{ "enabled": true }
```

Con `enabled: false` el plugin no se carga: los clientes deben enviar el header manualmente o no habra trazabilidad.

### Uso desde el cliente

**Dejar que el gateway genere el trace:**

```bash
curl -i http://localhost:8000/api/v1/login \
  -H "Content-Type: application/json" \
  -d '{"username":"...","password":"..."}'
```

El gateway añadira un `Traceparent` nuevo y lo enviara al backend. Visible en logs de gateway y backends.

**Enviar un trace propio (correlacion end-to-end):**

```bash
TRACE_ID=$(openssl rand -hex 16)
PARENT_ID=$(openssl rand -hex 8)
TRACEPARENT="00-${TRACE_ID}-${PARENT_ID}-01"

curl -i http://localhost:8000/api/v1/login \
  -H "Traceparent: ${TRACEPARENT}" \
  -H "Content-Type: application/json" \
  -d '{"username":"...","password":"..."}'
```

**Fallback con solo `Trace-Id` (32 hex, sin formato W3C):**

```bash
TRACE_ID=$(openssl rand -hex 16)

curl -i http://localhost:8000/api/v1/login \
  -H "Trace-Id: ${TRACE_ID}" \
  -H "Content-Type: application/json" \
  -d '{"username":"...","password":"..."}'
```

El gateway construira el `Traceparent` completo a partir de ese `trace-id`. Util si el front ya tiene un sistema propio de IDs y no quiere generar el formato W3C.

**Generar `trace-id` en JavaScript:**

```js
const hex = (n) => Array.from(crypto.getRandomValues(new Uint8Array(n)))
  .map(b => b.toString(16).padStart(2, "0")).join("");
const traceparent = `00-${hex(16)}-${hex(8)}-01`;
fetch("/api/v1/login", { headers: { Traceparent: traceparent } });
```

### Inspeccionar trazas en logs

```bash
make logs | grep -i traceparent
```

Cada backend Spring Boot que tenga Micrometer Tracing u OpenTelemetry configurado leera `Traceparent` entrante y lo propagara en sus propias trazas → correlacion completa entre gateway y microservicios.

### Notas

- CORS `allow_headers` esta configurado como `["*"]` — el gateway acepta cualquier header entrante. No hay filtrado por nombre a nivel CORS.
- `expose_headers` actual: `Content-Length`, `Content-Type`, `Traceparent`, `Tracestate`, `Trace-Id`. Solo estos son legibles por JS en el browser desde la respuesta.
- El `parent-id` (span-id) que genera el gateway representa el span del propio gateway. Los backends crearan spans hijos referenciando este valor.
- Flag `01` indica que la traza esta marcada para muestreo. Cambia a `00` para descartar.
- `X-Traceparent` lo añade el gateway al request hacia backend (no a la respuesta hacia el front). El cliente nunca lo envia ni lo recibe.
- Backends Spring Boot pueden mapear `X-Traceparent` directo al MDC (Logback `%X{X-Traceparent}`) sin parsear nada extra.
- `Trace-Id` solo se acepta si tiene exactamente 32 caracteres hex (`[0-9a-f]{32}`). Cualquier otro formato se ignora y el gateway genera uno nuevo.
- `Trace-Id` NO se reenvia al backend — solo se usa para construir `Traceparent`/`X-Traceparent`.

## Seguridad: estado y deuda tecnica conocida

> **Estado JWT**: el plugin `jwt-headers` esta **activo** (`jwt.json` → `enabled: true`) y valida tokens contra el JWKS de Keycloak. Los `skip_paths` eximen rutas publicas (auth, webhooks de pago, swagger, health). La introspection sigue **deshabilitada** (`introspection_enabled: false`).

> **Cabeceras de seguridad en el edge** (Clickjacking / MITM / MIME-sniffing): **implementadas** via modulo `security/http` (`security_headers.json` → `enabled: true`). Decision en [ADR-0001](docs/adr/0001-security-headers-edge.md).

Deuda pendiente antes de produccion: CORS wildcard + strip de headers de identidad en el plugin (ver abajo).

### Headers de identidad spoofeables

CORS actual: `allow_headers: ["*"]` — el gateway acepta **cualquier header del cliente**, incluyendo browsers (no hay barrera de preflight). El cliente puede enviar los siguientes headers y el gateway los reenvia al backend sin sobrescribir en rutas exentas (`skip_paths`), y el plugin aun no hace strip previo:

| Header | Origen previsto (con JWT enforced) | Riesgo actual |
|--------|------------------------------------|---------------|
| `x-user-id` | Plugin `jwt-headers` (claim `sub`) | Front puede enviarlo libremente desde browser, curl o mobile. Backend NO debe usarlo para autorizacion sin re-validar el JWT. |
| `x-user-roles` | Plugin `jwt-headers` (claim `realm_access.roles`) | Spoofeable desde browser, curl o mobile (CORS wildcard no filtra). |
| `x-username` | Plugin `jwt-headers` (claim `preferred_username`) | Idem. |
| `x-ip` | Plugin `jwt-headers` (`extractClientIP`) | Idem. |
| `x-org-id` | **Sin definir** — actualmente front directo | Multi-tenant break si backend lo usa para AuthZ. |

### Mitigaciones pendientes (TODO)

1. **Revisar `skip_paths`:** el plugin `jwt-headers` ya valida JWT, pero los `skip_paths` aun cubren rutas que algun dia tendran identidad. Auditar la lista periodicamente. Evaluar activar `introspection_enabled` para revocacion inmediata de tokens.
2. **Strip de identidad en plugin:** al inicio del handler, hacer `req.Header.Del(...)` para `x-username`, `x-user-roles`, `x-ip` (y `x-user-id` cuando JWT este enforced) **antes** de leer el JWT, evitando que el cliente fuerce valores en `skip_paths`.
3. **Mover `x-org-id` a claim JWT:** mientras venga del front es spoofeable. Configurar Keycloak para incluir `org_id` en el token y mapearlo via `claims_to_headers`.
4. **Backends defensivos:** cada microservicio Spring Boot debe re-validar el `Authorization: Bearer ...` y NO confiar en headers `x-user-*` salvo que provengan de un canal verificado (mTLS gateway↔backend, header firmado, etc).
5. **Restringir CORS `allow_headers`:** actualmente `["*"]`. Antes de produccion: listar explicitamente los headers permitidos (`Authorization`, `Content-Type`, `Accept`, `Traceparent`, `Trace-Id`, `x-recaptcha-*`, etc) y excluir los inyectados por plugins (`x-user-*`, `x-username`, `x-ip`, `x-geo-*`).

### Skip paths sensibles

`config/settings/jwt.json` exime de validacion las rutas listadas en `skip_paths` (por defecto solo `/public/*`). Cualquier ruta que agregues ahí queda **sin autenticación** — el gateway reenvía al backend cualquier header de identidad que mande el cliente. Regla general: todo endpoint público (webhooks de terceros, callbacks, login) debe **validar su propia firma/credencial en el backend** y nunca confiar en headers `x-user-*` que lleguen por una ruta exenta.

## Configuracion

La configuracion usa [KrakenD Flexible Configuration](https://www.krakend.io/docs/configuration/flexible-config/). Cada fichero `.json` en `settings/` se convierte en un namespace de variables en el template.

Ficheros disponibles: `service.json`, `hosts.json`, `cors.json`, `jwt.json`, `rate_limit.json`, `logging.json`, `metrics.json`, `ip_resolver.json`, `trace_context.json`, `accept_language.json`, `gateway_timeout.json`, `security_headers.json`, `tls.json`, `client_tls.json` (ver secciones **TLS / HTTPS** y **Cabeceras de seguridad**).

### Servicios backend

Defaults en `config/settings/hosts.json`. Cada host es override-able via envvar (ver **Apuntar el gateway a tus backends**).

| Servicio | Default | Envvar override |
|----------|---------|-----------------|
| Example | `http://example-service:8080` | `EXAMPLE_HOST` |

Resolucion en `krakend.tmpl`: `{{ env "EXAMPLE_HOST" | default .hosts.example }}`. Añade tus propios servicios agregando claves en `hosts.json` y su envvar correspondiente.

### Rate Limiting

Configurado en `config/settings/rate_limit.json`:

| Parametro | Valor | Descripcion |
|-----------|-------|-------------|
| `service_max_rate` | 500 | Limite global de requests/s |
| `service_client_max_rate` | 50 | Limite por cliente/s |
| `endpoint_max_rate` | 100 | Limite por endpoint/s |
| `endpoint_client_max_rate` | 20 | Limite por cliente por endpoint/s |
| `strategy` | `ip` | Estrategia de identificacion |

### JWT / Keycloak

Configurado en `config/settings/jwt.json` (plugin `jwt-headers`). Valida el `Authorization: Bearer` contra el JWKS del realm y mapea claims a headers (`x-username`, `x-user-roles`, `x-user-id`).

**Apuntar a tu realm** — `jwks_url` e `issuer` son override-ables por entorno (default en `jwt.json`, realm `example` de ejemplo):

```bash
KEYCLOAK_JWKS_URL=http://keycloak:8080/realms/mi-realm/protocol/openid-connect/certs \
KEYCLOAK_ISSUER=http://keycloak:8080/realms/mi-realm \
make up
```

**skip_paths** (rutas sin autenticación) soporta:

- **Match exacto**: `/api/v1/login`
- **Wildcard trailing**: `/public/*` (cubre cualquier ruta bajo `/public/`)
- **Wildcard de segmento**: `/api/v1/*/managers/*` (`*` = exactamente un segmento; ver tests en `plugins/jwt-headers/matchers_test.go`)

> Wildcard **prefijo** (`*/actuator/health`) NO funciona: los paths empiezan con `/` y el matcher no lo cubre. Lista cada ruta explícita o usa `/public/*`.

Default actual: `["/public/*"]`.

## Rutas publicas (`/public/*`)

Convencion para endpoints **sin autenticacion**: prefijo `/public/`. El bloque `/public/*` en `skip_paths` (jwt.json + ip_resolver.json) hace que cualquier ruta con ese prefijo evite validacion JWT y geo-lookup.

### Estrategia

1. **Frontend**: cliente llama `/public/<ruta>`.
2. **Gateway**: enruta al `backend.url_pattern` real que definas (ej. `/api/v1/health`).
3. **No-auth**: plugins JWT y IP-resolver saltean la ruta automaticamente via wildcard `/public/*`.
4. **Auditabilidad**: cualquier endpoint publico es identificable por el prefijo en logs.

### Anadir nueva ruta publica

1. Definir endpoint en `krakend.tmpl` con `endpoint: "/public/v1/..."` y `backend.url_pattern` apuntando al servicio real.
2. Quitar de `input_headers` los relacionados con auth: `Authorization`, `x-user-id`, `x-org-id`, `x-user-roles`, `x-username`.
3. No tocar `skip_paths` — `/public/*` ya cubre la ruta.
4. `make check` para validar config, `make plugin-build` si es la primera vez.

### Endpoints publicos actuales

| Frontend                | Backend (`EXAMPLE_HOST`)  | Metodo |
| ----------------------- | ------------------------- | ------ |
| `/public/v1/health`     | `/api/v1/health`          | GET    |

### Wildcard skip_paths (interno)

Plugins `jwt-headers` y `ip-resolver` interpretan las entradas de `skip_paths` así:

- `"/public/*"` (sufijo `/*`) → prefix-match: todas las rutas `/public/...`
- `"/api/v1/*/managers/*"` (`*` intermedio) → un segmento comodín por cada `*`
- `"/api/v1/login"` (sin `*`) → match exacto

Implementacion (`plugins/jwt-headers/main.go` · `buildMatchers`/`compilePattern`): entradas sin `*` van a un set de exact-match; las que tienen `*` se compilan a regex anclado (`*` → `[^/]+`, sufijo `/*` → `(/.*)?`).

## Puertos

| Puerto contenedor | Puerto local (compose) | Descripcion |
|-------------------|------------------------|-------------|
| **8080** | **8000** | API Gateway (HTTP/HTTPS segun `tls.json`) |
| **8090** | **8001** | Metricas (Prometheus) |

> KrakenD escucha en un unico puerto (`8080`). Con TLS activo ese mismo puerto sirve HTTPS — no hay puerto 8443 separado en `docker-compose.yml`; el acceso local es `https://localhost:8000`.

## TLS / HTTPS

Soporte de HTTPS con certificados gestionados por el usuario. **Estado actual: `tls.json` → `disabled: false` (TLS activo)**, `min_version: TLS12`, `max_version: TLS13`. Para servir HTTP plano, poner `disabled: true`.

### Activar HTTPS

1. Coloca los certificados en `./certs/` (montado como `/etc/krakend/certs` en el contenedor):
   - `certs/server.crt` — certificado publico (PEM)
   - `certs/server.key` — clave privada (PEM)
2. Edita `config/settings/tls.json` y cambia `disabled` a `false`.
3. Regenera y reinicia: `make generate && make down && make up`.
4. Verifica: `curl -k https://localhost:8443/__health`.

### Cert auto-firmado para desarrollo

```bash
make tls-dev-cert        # genera certs/server.{crt,key}, CN=localhost, 365 dias
make tls-clean           # borra certs (mantiene .gitkeep)
```

Variables: `TLS_CN=midominio.local TLS_DAYS=30 make tls-dev-cert`.

### Configuracion (`config/settings/tls.json`)

Schema homologado a [KrakenD config v3](https://www.krakend.io/schema/krakend.json) (`"version": 3`).

| Campo         | Tipo   | Descripcion                                                                                |
| ------------- | ------ | ------------------------------------------------------------------------------------------ |
| `disabled`    | bool   | `true` = gateway sirve HTTP plano. `false` = activa TLS.                                   |
| `keys`        | array  | **Requerido.** Lista de `{public_key, private_key}`. Soporta SNI multi-cert.               |
| `min_version` | string | `SSL3.0`, `TLS10`, `TLS11`, `TLS12`, `TLS13`. **Default KrakenD: `TLS13`** (rompe TLS1.2). |
| `max_version` | string | Mismos valores que `min_version`. Default `TLS13`.                                         |
| `enable_mtls` | bool   | Exige certificado de cliente (mTLS).                                                       |
| `ca_certs`    | array  | Rutas a CAs para validar clientes mTLS.                                                    |

Estructura de `keys[]` (cada entrada):

```json
{
  "public_key": "/etc/krakend/certs/server.crt",
  "private_key": "/etc/krakend/certs/server.key"
}
```

### Notas operativas

- **Puerto unico**: KrakenD escucha en un solo puerto (`service.port = 8080`). Activar TLS hace que ese mismo puerto sirva HTTPS — no coexisten HTTP y HTTPS simultaneamente. `docker-compose.yml` mapea `8000:8080`, asi que con TLS activo el gateway responde en `https://localhost:8000` (no hay puerto 8443 separado).
- **Endpoint de metricas (`8090`)**: listener separado, no hereda TLS. Si necesitas TLS en metricas, configuralo aparte.
- **Rotacion de certificados**: KrakenD carga los certs al arrancar. Cambiar el cert requiere reinicio del contenedor.
- **CORS**: si los clientes pasan de `http://` a `https://`, actualiza `allow_origins` en `config/settings/cors.json` para incluir el origen HTTPS.
- **mTLS**: `enable_mtls` + `ca_certs` activan validacion de cliente, pero la distribucion de certs de cliente queda fuera del alcance de este repo.
- **Secretos**: el directorio `certs/` esta en `.gitignore`. **Nunca** comitees claves privadas.

### Cliente TLS hacia backends (`config/settings/client_tls.json`)

Controla como el gateway valida certificados de backends HTTPS. Independiente del bloque `tls` server-side. Siempre se renderiza.

```json
{
  "@comment": "Skip SSL verification when connecting to backends",
  "allow_insecure_connections": false
}
```

| Campo                        | Tipo | Descripcion                                                                                                  |
| ---------------------------- | ---- | ------------------------------------------------------------------------------------------------------------ |
| `allow_insecure_connections` | bool | `true` = ignora verificacion SSL de backends (**solo dev**). `false` = valida cadena (recomendado).          |

Schema completo soporta tambien `ca_certs`, `client_certs[]` (mTLS hacia backend), `min_version`, `max_version`, `cipher_suites`, `curve_preferences`, `disable_system_ca_pool`. Anadir solo si necesario.

## Cabeceras de seguridad (`config/settings/security_headers.json`)

Modulo nativo `security/http` de KrakenD a nivel de servicio. Emite cabeceras de seguridad en **todas** las respuestas del gateway. Decision: [ADR-0001](docs/adr/0001-security-headers-edge.md).

| Amenaza        | Cabecera emitida                                | Campo                                                  |
| -------------- | ----------------------------------------------- | ------------------------------------------------------ |
| MITM/downgrade | `Strict-Transport-Security`                     | `hsts.seconds`, `hsts.include_subdomains`, `hsts.preload` |
| Clickjacking   | `X-Frame-Options: DENY` + CSP `frame-ancestors` | `frame_deny`, `content_security_policy`                |
| MIME-sniffing  | `X-Content-Type-Options: nosniff`               | `content_type_nosniff`                                 |

| Campo                     | Tipo   | Descripcion                                                                       |
| ------------------------- | ------ | -------------------------------------------------------------------------------- |
| `enabled`                 | bool   | `false` = no emite el bloque `security/http`.                                     |
| `frame_deny`              | bool   | `true` = `X-Frame-Options: DENY`.                                                 |
| `content_security_policy` | string | CSP. `frame-ancestors 'none'` cubre clickjacking en navegadores modernos.        |
| `content_type_nosniff`    | bool   | `true` = `X-Content-Type-Options: nosniff`.                                       |
| `browser_xss_filter`      | bool   | `true` = `X-XSS-Protection: 1; mode=block` (legacy).                              |
| `referrer_policy`         | string | Valor de `Referrer-Policy`.                                                       |
| `hsts.seconds`            | int    | `max-age` de HSTS. Solo se emite si TLS esta activo (`tls.disabled=false`).       |
| `hsts.include_subdomains` | bool   | Anade `includeSubDomains`.                                                        |
| `hsts.preload`            | bool   | Anade `preload`. **Mantener `false` en dev** — evita envenenar la cache HSTS de `localhost`. |

### HSTS por entorno

`hsts.seconds = 0` en dev (HSTS efectivamente desactivado). Override en cert/prod sin editar el JSON:

```bash
HSTS_SECONDS=31536000 make generate    # 1 año
```

El bloque HSTS solo se renderiza cuando `tls.disabled=false` (HSTS sobre HTTP plano lo ignora el navegador).

## Endpoints

El scaffold trae **2 endpoints de referencia** en `config/krakend.tmpl`. Reemplázalos por los tuyos.

| Endpoint | Metodo | Auth | Backend | Proposito |
|----------|--------|------|---------|-----------|
| `/api/v1/example` | GET | JWT (Keycloak) | `EXAMPLE_HOST` `/api/v1/example` | Ejemplo protegido: el plugin `jwt-headers` valida el token e inyecta `x-user-*` |
| `/public/v1/health` | GET | Ninguna (`/public/*`) | `EXAMPLE_HOST` `/api/v1/health` | Ejemplo público: exento de JWT y geo-lookup |

### Añadir un endpoint

1. Copia uno de los dos bloques de ejemplo en el array `endpoints` de `krakend.tmpl`.
2. Ajusta `endpoint` (ruta pública), `method`, y `backend.url_pattern` + `host` (`{{ env "TU_HOST" | default .hosts.tu_servicio }}`).
3. **Protegido** → deja los `input_headers` de identidad (`Authorization`, `x-user-*`, `x-geo-*`). **Público** → prefijo `/public/` y quita los headers de auth.
4. Registra el host en `hosts.json` (+ envvar en `docker-compose.yml`) si es un backend nuevo.
5. `make check` para validar. `make plugin-build` solo si tocaste código de plugins.

> Anatomía de un endpoint: `endpoint` (ruta expuesta) · `method` · `input_headers` (whitelist de headers que llegan al backend) · `backend.url_pattern` (ruta real) · `backend.host` (destino). `output_encoding: no-op` + `encoding: no-op` = passthrough sin transformar el body.
