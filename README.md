# Codehunters API Gateway (KrakenD)

API Gateway para la plataforma Codehunters construido con [KrakenD](https://www.krakend.io/) `2.13.1`.

## Estructura del Proyecto

```
codehunters-gw-krakend/
├── endpoints.yaml                    # Fuente de verdad de los endpoints (editar aqui)
├── cmd/
│   └── gen/                          # Generador Go: endpoints.yaml -> endpoints.json
├── config/
│   ├── krakend.tmpl                  # Template principal (Flexible Configuration)
│   └── settings/
│       ├── endpoints.json            # GENERADO — no editar a mano (make gen)
│       ├── service.json              # Nombre, puerto, timeouts
│       ├── hosts.json                # Hosts de los servicios backend
│       ├── cors.json                 # Configuracion CORS
│       ├── jwt.json                  # JWT/JWKS (Keycloak)
│       ├── ip_resolver.json          # Geolocalizacion de IP
│       ├── trace_context.json        # W3C Trace Context
│       ├── rate_limit.json           # Rate limiting
│       ├── logging.json              # Logging
│       ├── metrics.json              # Metricas y telemetria
│       └── security_headers.json     # Cabeceras de seguridad edge (HSTS, X-Frame, nosniff)
├── plugins/
│   ├── jwt-headers/                  # Plugin: validacion JWT + extraccion de claims
│   ├── ip-resolver/                  # Plugin: geolocalizacion de IP via ip-api.com
│   ├── trace-context/                # Plugin: propagacion W3C Trace Context
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
1. `plugin-build` - Compila los 3 plugins dentro de Docker (compatibles con Linux)
2. `up` - Levanta KrakenD con `docker-compose`, montando `config/` y `plugins/build/` como volumenes

Si solo cambiaste configuracion (sin tocar codigo de plugins):

```bash
docker compose restart
```

### Apuntar el gateway a microservicios locales

Los hosts de backend definidos en `config/settings/hosts.json` son **defaults**. Cada uno puede sobreescribirse via variable de entorno desde el shell o un `.env` junto al `docker-compose.yml`.

| Envvar | Default | Override hacia el host |
|--------|---------|-----------------------|
| `AUTH_HOST` | `http://codehunters-ms-auth:8080` | Servicio de autenticacion |
| `FILE_SHARE_HOST` | `http://codehunters-ms-file-share:8080` | Servicio de file share |
| `MANAGMENT_HOST` | `http://codehunters-ms-managment:8080` | Servicio de management |
| `NOTIFICATIONS_HOST` | `http://codehunters-ms-notifications:8080` | Servicio de notificaciones |
| `PAYMENT_HOST` | `http://codehunters-ms-payment:8080` | Servicio de pagos |
| `RAFFLES_HOST` | `http://codehunters-ms-raffles:8080` | Servicio de rifas |

Resolucion: el template hace `{{ env "AUTH_HOST" | default .hosts.auth }}` — si la envvar esta seteada, gana; si no, usa `hosts.json`. Compose ya las propaga al contenedor con sus defaults.

#### Caso 1 — Micro corriendo en host local (fuera de Docker)

Usar `host.docker.internal` para que KrakenD dentro del contenedor llegue al puerto del host:

```bash
# .env junto a docker-compose.yml
AUTH_HOST=http://host.docker.internal:9095
RAFFLES_HOST=http://host.docker.internal:9098
```

```bash
make down && make up
```

#### Caso 2 — Micro corriendo en otro container con docker-compose propio

Conectar el gateway a la red del micro (o viceversa) y apuntar al nombre del servicio:

```bash
# .env
AUTH_HOST=http://codehunters-ms-auth:9095

# unirse a la red externa donde vive el micro
docker network connect codehunters-ms-auth_default codehunters-gw-krakend-krakend-1
```

#### Caso 3 — Mix: gateway local + algunos micros remotos

```bash
AUTH_HOST=http://localhost:9095 \
RAFFLES_HOST=https://raffles.dev.codehunters.io \
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
AUTH_HOST=http://localhost:9095 \
RAFFLES_HOST=http://localhost:9098 \
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
| `make gen` | Regenera `config/settings/endpoints.json` desde `endpoints.yaml` |
| `make gen-check` | Falla si `endpoints.json` esta desincronizado con `endpoints.yaml` |
| `make check` | `gen-check` + valida la configuracion KrakenD |
| `make generate` | Genera el `krakend.json` final desde templates |
| `make clean` | Elimina artefactos generados |

## Endpoints (generador)

Los endpoints **no se escriben a mano** en `krakend.tmpl`. La fuente de verdad es
`endpoints.yaml`; un generador Go (`cmd/gen`) lo expande a
`config/settings/endpoints.json`, que el template recorre con `range` al renderizar.

```
endpoints.yaml  ──make gen──▶  config/settings/endpoints.json  ──FC range──▶  krakend.tmpl
     (editas)                       (generado, commiteado)                      (render)
```

`endpoints.json` **se commitea**: el gateway arranca sin toolchain de Go. Editarlo a
mano no sirve — el proximo `make gen` lo sobrescribe y `make check` falla por drift.

### Anadir o cambiar un endpoint

1. Editar `endpoints.yaml`.
2. `make gen` — regenera `endpoints.json` (falla con los errores de validacion si el YAML es invalido).
3. `make check` — valida drift + schema KrakenD.
4. Commitear `endpoints.yaml` **y** `config/settings/endpoints.json` juntos.

### Esquema de `endpoints.yaml`

```yaml
backends:                              # hosts logicos, referenciados por clave
  forgeos:
    host_default: http://host.docker.internal:8080
    host_env: FORGEOS_HOST             # envvar que lo sobreescribe en runtime

defaults:                              # aplicados a endpoints que omitan el campo
  output_encoding: no-op
  encoding: no-op
  timeout: null

endpoints:
  - path: /api/projects/{projectId}/stories   # ruta expuesta por el gateway
    method: GET                               # GET|POST|PUT|PATCH|DELETE
    backend: forgeos                          # clave declarada en backends
    auth: protected                           # public => entra en skip_paths del JWT
    input_headers: [Accept, Authorization]    # explicito, sin herencia
```

| Campo | Obligatorio | Descripcion |
|-------|-------------|-------------|
| `path` | si | Ruta expuesta. Debe empezar por `/` |
| `method` | si | `GET`, `POST`, `PUT`, `PATCH`, `DELETE` |
| `backend` | si | Clave de `backends` |
| `auth` | si | `public` o `protected`. `public` anade el path a `skip_paths` del plugin JWT |
| `input_headers` | si | Headers que llegan al backend. Explicito por endpoint (auditabilidad) |
| `url_pattern` | no | Ruta en el backend. Default: igual que `path` |
| `input_query_strings` | no | Query params reenviados |
| `timeout` | no | Override del timeout de servicio (ej. `3600s` para SSE) |
| `disable_host_sanitize` | no | `true` para streams SSE |
| `output_encoding` / `encoding` | no | Default: los de `defaults` |
| `rate_limit` | no | `max_rate`, `client_max_rate`, `strategy` por endpoint |

Los header sets repetidos se factorizan con anchors YAML (`&identity` / `*identity`).
Cualquier clave top-level `x-*` se ignora — es scaffolding del propio fichero.

### Validacion

`make gen` aborta y reporta **todos** los errores de una pasada: path sin `/` inicial,
metodo desconocido, backend no declarado, `input_headers` vacio, `auth` invalido y
endpoints duplicados (`method` + `path`).

Tres capas de validacion en total:

1. **Generador** — reglas de esquema (arriba).
2. **`make gen-check`** — drift entre YAML y JSON commiteado. Tambien corre en CI (job `endpoints-drift`).
3. **`krakend check`** — schema de KrakenD sobre el template renderizado.

### Rutas publicas y `skip_paths`

`skip_paths` del plugin JWT se compone en render-time como:

```
skip_paths estaticos (jwt.json)  +  todo endpoint con auth: public
```

Por eso `jwt.json` solo lleva entradas no-endpoint (globs como `/public/*`). Abrir una
ruta se hace **solo** poniendo `auth: public` en `endpoints.yaml` — nunca editando
`skip_paths` a mano. Asi ninguna ruta queda sin auth sin que se vea en el YAML.

### Uso directo del CLI

`make gen` es lo normal. Invocacion directa (el modulo vive en `cmd/gen`, sin go.mod raiz):

```bash
cd cmd/gen && go run . <entrada.yaml> <salida.json>
cd cmd/gen && go test ./...        # tests del generador
```

Los argumentos son obligatorios en la practica: los defaults del binario
(`endpoints.yaml` → `config/settings/endpoints.json`) se resuelven contra el
directorio actual, y el modulo obliga a ejecutar desde `cmd/gen`. Por eso el target
`gen` del Makefile pasa rutas absolutas (`$(CURDIR)/...`).

Diseno y decisiones: [`docs/superpowers/specs/2026-08-03-krakend-config-generator-design.md`](docs/superpowers/specs/2026-08-03-krakend-config-generator-design.md).

## Plugins

El gateway utiliza varios plugins custom registrados como HTTP server middleware:

| Plugin | Descripcion | Settings |
|--------|-------------|----------|
| **gateway-timeout** | Envuelve toda la cadena (incluido el backend); convierte un `5xx` en `504` tras `min_elapsed` transcurrido | `gateway_timeout.json` |
| **accept-language** | Aplica un `Accept-Language` por defecto cuando el cliente no lo envia | `accept_language.json` |
| **trace-context** | Propaga headers W3C Traceparent/Tracestate. Genera trace IDs si no existen | `trace_context.json` |
| **ip-resolver** | Resuelve IP del cliente a geolocalizacion (pais, ciudad, coordenadas) via ip-api.com con cache | `ip_resolver.json` |
| **session-resolver** | Lee la sesion `v1:session:{sid}` de Valkey (escrita por `auth-bff`) y la convierte en `Authorization: Bearer` antes de `jwt-headers`. Bearer entrante siempre pasa sin tocar (modo dual browser/MCP/CI) | `session.json` |
| **jwt-headers** | Valida JWT contra JWKS de Keycloak y mapea claims a headers HTTP (x-username, x-user-roles, x-user-id) | `jwt.json` |

Orden de ejecucion real (izquierda = primero en ver la peticion):
`gateway-timeout → accept-language → trace-context → ip-resolver → session-resolver → jwt-headers`.
KrakenD ejecuta el array `plugin/http-server.name` de `config/krakend.tmpl`
en el orden **inverso** al declarado — ver la seccion de orden de cadena en
[`docs/session-flow.md`](docs/session-flow.md) antes de tocar ese array.

`session-resolver` implementa el patron Token Handler / BFF: el navegador solo
ve una cookie `HttpOnly`, nunca el JWT. Orden de cadena, los cuatro flujos
(login, request autenticado, refresh perezoso, logout), el contrato de
Valkey y el runbook completo estan en
[`docs/session-flow.md`](docs/session-flow.md).

### Habilitar / Deshabilitar plugins

Cada plugin tiene un campo `enabled` en su fichero de settings:

```json
// config/settings/trace_context.json
{ "enabled": true }

// config/settings/ip_resolver.json
{ "enabled": false, ... }

// config/settings/jwt.json
{ "enabled": true, ... }
```

Cambia `"enabled"` a `true` o `false` y reinicia el gateway.

### Headers inyectados por plugins

| Header | Plugin | Descripcion |
|--------|--------|-------------|
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

## Seguridad: deuda tecnica conocida

> **Aviso**: el gateway actualmente **NO valida JWT** de forma estricta. Esta seccion documenta los riesgos pendientes para que sean tratados antes de produccion.

> **Cabeceras de seguridad en el edge** (Clickjacking / MITM / MIME-sniffing): pendiente de implementar via modulo `security/http`. Decision en [ADR-0001](docs/adr/0001-security-headers-edge.md).

### Headers de identidad spoofeables

CORS actual: `allow_headers: ["*"]` — el gateway acepta **cualquier header del cliente**, incluyendo browsers (no hay barrera de preflight). El cliente puede enviar los siguientes headers y el gateway los reenvia al backend sin sobrescribir cuando JWT no esta enforced:

| Header | Origen previsto (con JWT enforced) | Riesgo actual |
|--------|------------------------------------|---------------|
| `x-user-id` | Plugin `jwt-headers` (claim `sub`) | Front puede enviarlo libremente desde browser, curl o mobile. Backend NO debe usarlo para autorizacion sin re-validar el JWT. |
| `x-user-roles` | Plugin `jwt-headers` (claim `realm_access.roles`) | Spoofeable desde browser, curl o mobile (CORS wildcard no filtra). |
| `x-username` | Plugin `jwt-headers` (claim `preferred_username`) | Idem. |
| `x-ip` | Plugin `jwt-headers` (`extractClientIP`) | Idem. |
| `x-org-id` | **Sin definir** — actualmente front directo | Multi-tenant break si backend lo usa para AuthZ. |

### Mitigaciones pendientes (TODO)

1. **Activar enforcement JWT real:** plugin `jwt-headers` valida formato pero los `skip_paths` aun cubren rutas que algun dia tendran identidad.
2. **Strip de identidad en plugin:** al inicio del handler, hacer `req.Header.Del(...)` para `x-username`, `x-user-roles`, `x-ip` (y `x-user-id` cuando JWT este enforced) **antes** de leer el JWT, evitando que el cliente fuerce valores en `skip_paths`.
3. **Mover `x-org-id` a claim JWT:** mientras venga del front es spoofeable. Configurar Keycloak para incluir `org_id` en el token y mapearlo via `claims_to_headers`.
4. **Backends defensivos:** cada microservicio Spring Boot debe re-validar el `Authorization: Bearer ...` y NO confiar en headers `x-user-*` salvo que provengan de un canal verificado (mTLS gateway↔backend, header firmado, etc).
5. **Restringir CORS `allow_headers`:** actualmente `["*"]`. Antes de produccion: listar explicitamente los headers permitidos (`Authorization`, `Content-Type`, `Accept`, `Traceparent`, `Trace-Id`, `x-recaptcha-*`, etc) y excluir los inyectados por plugins (`x-user-*`, `x-username`, `x-ip`, `x-geo-*`).

### Skip paths sensibles

`config/settings/jwt.json` exime de validacion: webhooks de pago (`smartfastpay`, `tumipay`, `mock`), endpoints publicos de auth (`login`, `register`, `refresh`, `verify/email`, `forgot-password`, `reset-password`), swagger y actuator/health. Webhooks de pago especialmente: cualquier cliente externo puede invocarlos enviando headers de identidad arbitrarios. **Validar firma de webhook (`SmartFastPay-Signature`, `x-trx-signature`) en backend es obligatorio.**

## Configuracion

La configuracion usa [KrakenD Flexible Configuration](https://www.krakend.io/docs/configuration/flexible-config/). Cada fichero `.json` en `settings/` se convierte en un namespace de variables en el template.

Ficheros disponibles: `service.json`, `hosts.json`, `cors.json`, `jwt.json`, `session.json` (ver [`docs/session-flow.md`](docs/session-flow.md)), `rate_limit.json`, `logging.json`, `metrics.json`, `ip_resolver.json`, `trace_context.json`, `tls.json`, `client_tls.json` (ver seccion **TLS / HTTPS**).

### Servicios backend

Defaults en `config/settings/hosts.json`. Cada host es override-able via envvar (ver **Apuntar el gateway a microservicios locales**).

| Servicio | Default | Envvar override |
|----------|---------|-----------------|
| Auth | `http://codehunters-ms-auth:8080` | `AUTH_HOST` |
| File Share | `http://codehunters-ms-file-share:8080` | `FILE_SHARE_HOST` |
| Management | `http://codehunters-ms-managment:8080` | `MANAGMENT_HOST` |
| Notifications | `http://codehunters-ms-notifications:8080` | `NOTIFICATIONS_HOST` |
| Payment | `http://codehunters-ms-payment:8080` | `PAYMENT_HOST` |
| Raffles | `http://codehunters-ms-raffles:8080` | `RAFFLES_HOST` |

Resolucion en `krakend.tmpl`: `{{ env "AUTH_HOST" | default .hosts.auth }}`.

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

Configurado en `config/settings/jwt.json`. Los paths en `skip_paths` no requieren
autenticacion; la lista final se compone en render-time con los estaticos de este
fichero mas los endpoints marcados `auth: public` en `endpoints.yaml` (ver
**Endpoints (generador)**). Soporta:

- **Match exacto**: `/codehunters-ms-auth/api/v1/login`
- **Wildcard prefijo**: `/public/*` (cubre cualquier ruta bajo `/public/`)

Ejemplos actuales:

- `/public/*` (todas las rutas publicas — ver seccion **Rutas publicas**)
- `/codehunters-ms-auth/api/v1/login`, `/codehunters-ms-auth/api/v1/accounts/register`
- `/codehunters-ms-auth/api/v1/token/refresh`, `/codehunters-ms-auth/api/v1/verify/email`
- `*/v1/api-docs` (documentacion OpenAPI), `*/actuator/health`

## Rutas publicas (`/public/*`)

Convencion para endpoints **sin autenticacion**: prefijo `/public/`. El bloque `/public/*` en `skip_paths` (jwt.json + ip_resolver.json) hace que cualquier ruta con ese prefijo evite validacion JWT y geo-lookup.

### Estrategia

1. **Frontend**: cliente llama `/public/<ruta>`.
2. **Gateway**: enruta al backend interno `/codehunters-ms-<servicio>/<ruta-real>`.
3. **No-auth**: plugins JWT y IP-resolver saltean la ruta automaticamente via wildcard `/public/*`.
4. **Auditabilidad**: cualquier endpoint publico es identificable por el prefijo en logs.

### Anadir nueva ruta publica

1. Anadir el endpoint en `endpoints.yaml` con `path: /public/v1/...`, `auth: public` y `url_pattern` apuntando a la ruta real del backend.
2. Quitar de `input_headers` los relacionados con auth: `Authorization`, `x-user-id`, `x-org-slug`, `X-Organization-Id`, `x-user-roles`, `x-username`.
3. No tocar `skip_paths` en `jwt.json` — `auth: public` lo deriva, y `/public/*` ya cubre el prefijo.
4. `make gen` y luego `make check` para validar; `make plugin-build` si es la primera vez.

### Endpoints publicos actuales

| Frontend                                                                  | Backend                                                                                          | Metodo |
| ------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------ | ------ |
| `/public/v1/shared/{reference-id}/documents/{workflow-id}/presigned-url`  | `codehunters-ms-file-share` `/api/v1/shared/{reference-id}/documents/{workflow-id}/presigned-url`      | POST   |

### Wildcard skip_paths (interno)

Plugins `jwt-headers` y `ip-resolver` interpretan entradas con sufijo `/*` como prefix-match:

- `"/public/*"` → match todas las rutas `/public/...`
- `"/codehunters-ms-auth/api/v1/login"` → match exacto (sin sufijo)

Implementacion: al cargar config, las entradas `/*` se separan en lista de prefijos; en cada request se evalua exact-match O prefix-match con `strings.HasPrefix`.

## Puertos

| Puerto | Descripcion |
|--------|-------------|
| **8080** (8000 en local) | API Gateway (HTTP) |
| **8443** (8443 en local) | API Gateway (HTTPS, opcional — ver TLS / HTTPS) |
| **9090** (8001 en local) | Metricas (Prometheus) |

## TLS / HTTPS

Soporte opcional de HTTPS con certificados gestionados por el usuario. Por defecto el flag esta `disabled: true` (gateway escucha solo HTTP) y no requiere ningun cambio operativo.

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

- **Puerto unico**: KrakenD escucha en un solo puerto (`service.port = 8080`). Activar TLS hace que ese mismo puerto sirva HTTPS — no coexisten HTTP y HTTPS simultaneamente. El mapeo `8443:8080` en `docker-compose.yml` solo es relevante con TLS activo.
- **Endpoint de metricas (`9090`)**: listener separado, no hereda TLS ni comparte puerto con el servicio principal (`config/settings/metrics.json`). Si necesitas TLS en metricas, configuralo aparte.
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

El gateway expone 57 endpoints agrupados por servicio:

### Auth Service (`codehunters-ms-auth`)
- **POST** `/api/v1/login` - Login
- **POST** `/api/v1/logout` - Logout
- **POST** `/api/v1/accounts/register` - Registro de cuenta
- **POST** `/api/v1/accounts/change-password` - Cambio de password
- **POST** `/api/v1/token/refresh` - Refresh token
- **POST** `/api/v1/verify/email` - Verificacion de email
- **GET** `/api/v1/document/validate` - Validar documento
- **GET/POST** `/api/v1/{user-id}/managers/*` - Gestion de managers
- **POST** `/api/v1/{user-id}/bettors/register` - Registro de apostadores
- **CRUD** `/api/v1/admin/roles` - Gestion de roles
- **CRUD** `/api/v1/admin/permissions` - Gestion de permisos
- **CRUD** `/api/v1/admin/roles/{roleId}/permissions` - Permisos por rol
- **CRUD** `/api/v1/admin/users/{userId}/roles` - Roles por usuario
- **GET** `/api/v1/admin/users/{userId}/permissions` - Permisos de usuario
- **GET** `/v1/api-docs` - Documentacion OpenAPI

### File Share Service (`codehunters-ms-file-share`)
- **GET** `/api/v1/shared/{reference-id}/documents` - Listar documentos
- **GET** `/api/v1/shared/{reference-id}/documents/{workflow-id}` - Obtener documento
- **POST** `/api/v1/shared/{reference-id}/documents/{workflow-id}/upload` - Subir documento

### Raffles Service (`codehunters-ms-raffles`)
- **POST** `/api/v1/raffles` - Crear rifa
- **PATCH** `/api/v1/raffles/{raffleId}/info` - Actualizar info
- **POST** `/api/v1/raffles/{raffleId}/submit-for-review` - Enviar a revision
- **GET** `/api/v1/raffles/{raffleId}/documents` - Documentos de rifa
- **POST** `/api/v1/raffles/{raffleId}/documents/{documentId}/review` - Revisar documento
- **POST** `/api/v1/raffles/viability/*` - Calculos de viabilidad
- **POST** `/api/v1/raffles/{raffleId}/prizes` - Gestionar premios
- **GET/PATCH/POST** `/api/v1/raffles/{raffleId}/review` - Revisiones
- **GET** `/v1/api-docs` - Documentacion OpenAPI

### Management Service (`codehunters-ms-managment`)
- Mismos endpoints de raffles replicados para gestion administrativa
- **GET/POST/PATCH** `/admin/uvt-config` - Configuracion UVT
- **GET** `/v1/api-docs` - Documentacion OpenAPI
