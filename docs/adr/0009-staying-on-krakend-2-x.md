---
status: accepted
date: 2026-10-18
accepted-date: 2026-10-18
decision-makers: [Carlos Andres Montoya Tobon]
consulted: [equipo plataforma, seguridad]
informed: [equipo forgeos, auth-bff]
---

# ADR-0009: El gateway se queda en KrakenD 2.x mientras CE 3 no cargue plugins

Formato Nygard. La decisión se tomó el 2026-10-18 al evaluar la subida a `3.0.1`, y lo que
la hace digna de registro no es la versión: es que el modo de fallo de equivocarse es
silencioso. Se conserva el frontmatter del registro y se añade `Confirmation`.

## Context

KrakenD CE publicó `3.0.0` y `3.0.1`, y `krakend:latest` ya apunta a `3.0.1`. El repositorio
está en `2.13.4`.

**KrakenD CE 3.0 eliminó el soporte de plugins Go.** Esto es el gateway entero: cinco
plugins —`jwt-headers`, `session-resolver`, `trace-context`, `accept-language`,
`gateway-timeout`— y ninguna otra pieza hace su trabajo. No hay `auth/validator` en
`config/krakend.tmpl`: **toda** la validación de JWT vive en `jwt-headers`, junto con la
introspección, la puerta de roles por ruta y el borrado anti-spoof de las cabeceras
`x-user-*` que ADR-0008 enumera como no sustituibles.

Medido el 2026-10-18, misma configuración contra los dos binarios:

```
krakend:2.13.4
  DEBUG: [SERVICE: Plugin Loader] Starting loading process
  DEBUG: [SERVICE: Handler Plugin] open /opt/krakend/plugins/...
  DEBUG: [PLUGIN: Server] No plugins registered for the module

krakend:3.0.1
  INFO: Starting the KrakenD instance
  INFO: [SERVICE: Gin] Listening on port: 8099
```

En `3.0.1` **no existe el cargador de plugins**. Los subcomandos también desaparecieron:
`2.13.4` trae `check-plugin` y `test-plugin`; `3.0.1` solo `check` y `run`.

**La forma del fallo es lo grave.** `3.0.1` acepta los bloques `plugin` y
`plugin/http-server` sin protestar, `krakend check` responde `Syntax OK!` y cuenta
`1 global component configuration(s)`, el servidor arranca limpio — y no carga nada. Un
gateway así sirve **todas las rutas sin autenticar**: sin verificación de firma, sin
introspección, sin resolución de sesión por cookie, sin inyección de identidad. Es
configuración muerta que se lee como protección, el modo de fallo que este repositorio
rechaza por principio.

Lo demás que rompe 3.0, para el registro:

| Cambio de 3.0 | Nos afecta |
|---|---|
| Plugins Go fuera de CE | **Bloqueante** |
| `"version": 3` → `4` obligatorio | Sí, mecánico. Falla con `unsupported version: 3 (want: 4)` |
| `telemetry/opencensus` y sus exportadores eliminados | No, no se usan |
| `telemetry/influx` eliminado | No |
| Namespaces legacy (`github_com/devopsfaith/...`) | No, el template usa nombres cortos |
| Go del binario 1.25.9 → 1.26.9 | Mecánico: `plugins/Dockerfile.builder` y rebuild |

Las notas de 3.0 afirman además que se eliminó el puerto de telemetría. **Es falso en
`3.0.1`**: se montó `telemetry/metrics` con `listen_address` propio y `/__stats` respondió
`200`. Queda anotado porque un bloqueo inventado es tan caro como uno pasado por alto.

Las tres salidas que ofrece el proyecto son migrar a Enterprise, compilar un binario propio
sobre Lura con la lógica dentro, o quedarse en 2.x.

## Decision

**El gateway se queda en KrakenD 2.x, con pines exactos `x.y.z`.** No se migra a 3.x.

No se adopta Enterprise ni se construye un binario propio sobre Lura: ninguna de las dos es
una subida de versión, y nada obliga hoy a elegir entre ellas.

Se añade `scripts/check-krakend-major.sh` a `make check` para que la decisión sea ejecutable
y no prosa: falla si cualquier pin es `>= 3.x` o si es un tag flotante, mientras el template
declare plugins.

## Consequences

**Lo que se conserva.** Todo lo que ADR-0008 enumera, que es la razón de ser del edge. La
cadena de ADR-0003, 0004 y 0008 sigue en pie sin tocar una línea.

**La línea 2.x sigue viva y se puede subir dentro de ella.** `2.13.11` existe y mantiene los
subcomandos de plugin; el config actual valida contra ella sin cambios (`Syntax OK!`, con
`"version": 3`). Cuesta alinear el toolchain: su binario va con Go `1.26.8` contra el
`1.25.9` de `2.13.4`, así que `plugins/Dockerfile.builder` sube y los cinco `.so` se
recompilan. `make plugins-abi` lo verifica solo. Esa subida no se hizo en este cambio: se
decidió no migrar, y queda disponible.

**El riesgo que se acepta: no se conoce el fin de soporte de 2.x.** Ni las notas de 3.0 ni
la guía de migración dicen hasta cuándo recibe parches de seguridad. Mientras no haya fecha,
quedarse es la opción barata; cuando la haya, la elección entre Enterprise y Lura deja de ser
aplazable y necesita un ADR que supersede a este.

**El coste de Lura, para cuando toque estimarlo.** Es viable: ninguno de los cinco `go.mod`
importa librerías de KrakenD —solo stdlib, `keyfunc`, `valkey-go` y `testcontainers`— así que
la lógica es portable y el acoplamiento es el símbolo `RegisterHandlers`, no una API. Lo que
cambia es la propiedad: se pasa de consumir una imagen oficial a mantener un gateway propio,
con su build, sus releases y sus parches. Es un proyecto, no un bump.

**Un tag flotante es la trampa concreta.** `krakend:latest` ya resuelve a `3.0.1`. Hoy no hay
ninguno en el repositorio —los cinco pines son exactos— y el guard existe para que siga
siendo verdad sin depender de que alguien lo recuerde.

**Si algún día se quitan los plugins, esta decisión caduca sola.** El guard lee el template:
sin `plugin/http-server` no restringe nada y lo dice. Un repositorio sin plugins puede subir
a 3.x, y eso exigirá un ADR que supersede a este.

## Confirmation

- **`scripts/check-krakend-major.sh`** en `make check` (capa 3) y en el target
  `krakend-major`. Mutation testing el 2026-10-18, con los dos controles negativos: pin
  explícito a `3.0.1` en compose y en el `Makefile` → falla; `krakend/builder:3.0.1-linux-generic`
  → falla; tags flotantes `latest` y `2.13-linux-generic` → fallan; template sin
  `plugin/http-server` → pasa diciendo que no restringe; sin ningún pin visible → falla en vez
  de pasar en vacío.
- **`make plugins-loaded`** (`scripts/check-plugins-loaded.sh`) es la segunda red, contra un
  contenedor en marcha: exige la línea `"msg":"plugin loaded"` de cada plugin. En `3.0.1` no
  existe ninguna, así que falla. Es el gate que convierte este fallo silencioso en ruidoso si
  el pin llegara a 3.x por una vía que el guard no ve.
- **`make plugins-abi`** sigue siendo lo que ata el toolchain del builder al de la imagen, y
  es lo que haría ruido en cualquier subida dentro de 2.x.

## More Information

- [Notas de la versión de KrakenD CE 3.0](https://www.krakend.io/blog/krakend-3.0-release-notes/)
  y la [guía de actualización](https://www.krakend.io/docs/configuration/migrating/).
- Related: [ADR-0008](0008-the-edge-keeps-its-own-jwt-plugin.md) — las cinco propiedades del
  plugin propio que nada nativo sustituye. Esta decisión las protege de desaparecer por una
  subida de versión.
- Related: [ADR-0004](0004-plugin-chain-order.md) — el orden de la cadena que 3.x ignoraría
  junto con los plugins.
- La confusión de versiones del binario local (`unsupported version: 3 (want: 4)` al tener
  3.x instalado sobre un repositorio sano) ya estaba documentada en
  [`README.md`](../../README.md), sección de capas de validación. Es un problema distinto de
  este: ese es de herramienta local, este es del pin del repositorio.
