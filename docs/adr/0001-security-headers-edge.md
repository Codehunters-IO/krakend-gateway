---
status: proposed
date: 2026-06-26
decision-makers: [Carlos Andres Montoya Tobon]
consulted: [equipo plataforma, seguridad]
informed: [equipo frontend]
---

# ADR-0001: Cabeceras de seguridad en el edge vía módulo declarativo `security/http` de KrakenD

## Context and Problem Statement

El gateway (KrakenD 2.13.x) termina TLS y enruta todo el tráfico público hacia los
microservicios backend. Hoy no emite cabeceras de seguridad HTTP, dejando a los clientes
expuestos a tres vectores en el borde:

- **Clickjacking** — respuestas embebibles en `<iframe>` de terceros.
- **MITM / downgrade** — sin HSTS, el navegador puede ser forzado a HTTP plano.
- **MIME-sniffing** — el navegador reinterpreta el `Content-Type` y ejecuta contenido.

¿Dónde y cómo se deben inyectar `Strict-Transport-Security`, `X-Frame-Options` +
CSP `frame-ancestors`, y `X-Content-Type-Options` de forma consistente para todas las rutas?

## Decision Drivers

- Cobertura uniforme de **todas** las rutas sin tocar cada endpoint.
- El gateway es *dumb glue*: configuración declarativa > código imperativo.
- Sin lógica de negocio ni plugins Go salvo que lo declarativo no alcance.
- Reversible y parametrizable por entorno (dev self-signed vs cert/prod).
- Defensa en profundidad: el edge no exime al servicio de sus propias cabeceras.
- Cero coste de build adicional (no recompilar `.so`).

## Considered Options

1. **Módulo `security/http` de KrakenD a nivel de servicio** (declarativo, nativo).
2. **Plugin Go `http-server` propio** que setee las cabeceras.
3. **Cabeceras por endpoint** vía `response_headers`/modifier en cada ruta.
4. **Delegar a cada microservicio** (Spring Security headers) sin acción en el edge.
5. **Cabeceras en CDN/WAF/Load Balancer** aguas arriba del gateway.

## Decision Outcome

Opción elegida: **"Módulo `security/http` a nivel de servicio"**, porque cubre las tres
amenazas con una sola configuración declarativa, sin plugins ni build, aplica a todas las
rutas, y es parametrizable por entorno mediante el patrón de settings + env-override ya
usado en el repo.

Mapeo amenaza → cabecera → campo del módulo:

| Amenaza        | Cabecera                                          | Campo `security/http`                                   |
|----------------|---------------------------------------------------|--------------------------------------------------------|
| MITM/downgrade | `Strict-Transport-Security`                       | `sts_seconds`, `sts_include_subdomains`, `sts_preload` |
| Clickjacking   | `X-Frame-Options: DENY` + CSP `frame-ancestors`   | `frame_deny`, `content_security_policy`                |
| MIME-sniffing  | `X-Content-Type-Options: nosniff`                 | `content_type_nosniff`                                  |

Configuración externalizada en `config/settings/security_headers.json`; HSTS condicionado
a `{{- if not .tls.disabled }}` y `sts_preload:false` en dev para no envenenar la caché
HSTS de `localhost`.

### Consequences

- Bueno: una fuente única de cabeceras de seguridad; aplica a todo el tráfico.
- Bueno: cero plugins nuevos, cero recompilación, reversible vía flag/env.
- Bueno: alineado con la regla "declarativo > imperativo" del gateway.
- Malo: el módulo es service-scoped — no permite valores distintos por endpoint
  (aceptable; las 3 cabeceras son globales por diseño).
- Malo: si en el futuro una ruta necesita CSP relajada (p.ej. embeber un widget),
  habrá que escalar a modifier por endpoint → nuevo ADR.
- Neutro: no exime a los microservicios de re-emitir cabeceras (defensa en profundidad).

### Confirmation

- **Test de humo en CI** (`pull-request.yml`): `curl -kI https://gateway/__health`
  debe devolver las 3 cabeceras con los valores esperados.
- **`krakend check -d -t -c krakend.json`** valida render del template + schema.
- **Métrica/alerta opcional**: monitor sintético externo que verifique presencia de HSTS
  en prod (rotura = page).

## Pros and Cons of the Options

### 1. Módulo `security/http` (elegido)
- Bueno: nativo, declarativo, una línea por cabecera, cobertura total.
- Bueno: sin build; parametrizable por entorno.
- Malo: granularidad solo a nivel servicio.

### 2. Plugin Go propio
- Bueno: control total, lógica condicional arbitraria.
- Malo: reinventa lo que el módulo ya hace; build Go atado a versión de imagen;
  superficie de mantenimiento; viola "plugins solo como escape hatch".

### 3. Cabeceras por endpoint
- Bueno: granularidad fina por ruta.
- Malo: repetición en decenas de endpoints; deriva garantizada; fácil olvidar rutas nuevas.

### 4. Delegar a microservicios
- Bueno: defensa en profundidad real.
- Malo: no cubre respuestas del propio gateway (errores, health, swagger); inconsistencia
  entre servicios; no es responsabilidad de edge cedida. (Se mantiene **además**, no en lugar de.)

### 5. CDN/WAF aguas arriba
- Bueno: punto único más externo.
- Malo: no todos los entornos tienen CDN (dev/local sin cobertura); acopla seguridad a infra
  no versionada en este repo; HSTS depende de terminación TLS del LB. Complementario, no sustituto.

## More Information

- Patrón de settings + env-override: ver `README.md` y `config/settings/`.
- Plan de implementación: `security_headers.json` → `krakend.tmpl` (root `extra_config`,
  tras `telemetry/metrics`) → docs → validación.
- Riesgo dev HSTS: `sts_preload:false` y `sts_seconds` bajo en `service`/dev.
