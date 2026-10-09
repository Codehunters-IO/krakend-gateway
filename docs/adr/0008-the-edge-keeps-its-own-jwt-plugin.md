---
status: accepted
date: 2026-10-09
accepted-date: 2026-10-09
decision-makers: [Carlos Andres Montoya Tobon]
consulted: [equipo plataforma, seguridad]
informed: [equipo forgeos, auth-bff]
---

# ADR-0008: El edge se queda con su propio plugin JWT, no con el validador declarativo

Formato Nygard: la decisión estaba tomada de hecho y lo que faltaba era registrarla, con
las consecuencias que ya son visibles en el repositorio. Se conserva el frontmatter del
registro y se añade `Confirmation`, que Nygard no contempla y este registro exige.

## Context

KrakenD trae un módulo declarativo, `auth/validator`, que valida firma contra JWKS,
comprueba `iss` y `aud`, y con `propagate_claims` mapea claims a cabeceras. Eso es, sobre el
papel, la mayor parte de lo que hace `plugins/jwt-headers` — 624 líneas de Go, 37 tests, un
`.so` que hay que compilar con el mismo toolchain que el binario de KrakenD.

La pregunta de si sustituir el plugin por el módulo nativo lleva abierta desde la época de
ADR-0002, y el prerrequisito que se identificó para responderla era un spike: averiguar si
`propagate_claims` **borra** una cabecera que el cliente envía y el claim no produce.

Esa pregunta importa más de lo que parece. Lo que hace confiables las cabeceras `x-user-*`
río abajo no es que el plugin las escriba: es que las **borra primero**, todas las que
gestiona, y lo hace antes de los early returns de preflight y de `skip_paths`. Un header de
identidad solo puede existir si nace de un JWT validado en esa misma petición, incluso en una
ruta pública. El módulo nativo documenta que propaga claims; no documenta que limpie lo que
el cliente envió.

## Decision

**El edge se queda con `plugins/jwt-headers`.** El spike de `propagate_claims` se retira: no
se va a migrar, así que la pregunta que iba a responder dejó de condicionar nada.

## Consequences

**Lo que se conserva, y no es sustituible pieza por pieza.** Cinco propiedades del plugin que
el módulo nativo no ofrece, o no se sabe si ofrece:

1. **Anti-spoof por borrado previo.** Descrito arriba. Es la propiedad de la que depende que
   un backend pueda creerse `x-user-id`.
2. **Puerta de roles por ruta con claim por regla.** `required_roles` casa globs de ruta y
   cada regla nombra su propio claim, que es lo que permite exigir roles de **cliente**
   (`resource_access.<client>.roles`) en vez de roles de realm. ADR-0006 se apoya enteramente
   en eso; el módulo nativo no tiene puerta de roles por ruta.
3. **Introspección con caché por `jti`.** Revocación inmediata, que es el driver dominante de
   ADR-0003. El módulo nativo valida la firma y la expiración; un token revocado sigue siendo
   válido hasta que expira.
4. **Fail-closed al arrancar.** Mientras el JWKS no ha cargado, toda ruta protegida recibe
   `503` en vez de pasar.
5. **Cabeceras correctas en la denegación.** `application/json`, `no-store` y `nosniff`, que
   el módulo `security/http` no puede poner porque los plugins responden antes que él.

**Lo que cuesta, y hay que asumirlo.**

- Es código propio: 624 líneas que mantener, y 37 tests que mantenerlos honestos. Dos de esos
  tests existen porque el primer intento de probar la confusión de algoritmos **pasaba con
  `WithValidMethods` quitado** — lo descubrió mutation testing, no la revisión.
- El `.so` queda atado al toolchain Go del binario de KrakenD. `make plugins-abi` compara los
  dos pines, y es un guardia que no existiría con el módulo nativo.
- **Un plugin que no carga deja el edge abierto.** Medido el 2026-10-09: con la carpeta de
  plugins vacía, KrakenD arranca, registra cero plugins y sirve todo sin validar nada. El
  módulo nativo no tiene ese modo de fallo, porque va dentro del binario. Es el coste más
  serio de esta decisión y por eso existe `make plugins-loaded`.

**Lo que se gana en claridad.** La pregunta sale del registro. Nadie tiene que volver a
estimar el spike ni a comparar tablas de capacidades; si alguien quiere reabrirla, lo que
tiene que traer es una respuesta a las cinco propiedades de arriba, no una comparación
genérica de features.

## Confirmation

- **La suite del plugin**: 37 funciones `Test*` en `plugins/jwt-headers/`, en CI vía
  `make plugins-test` en cada pull request. Cubren firma, `iss`, algoritmo fuera del
  allowlist contra un JWK que no fija `alg`, claims requeridos, la puerta de roles con claim
  por regla, el modo observación y las cabeceras de la denegación.
- **`make plugins-abi`**: falla si el toolchain del builder se desvía del de la imagen.
- **`make plugins-loaded`**: falla si el gateway en marcha no registró los cinco plugins, que
  es el guardia del modo de fallo abierto que esta decisión acepta.
- **`scripts/check-plugin-chain-order.sh`**: el orden de la cadena y el acoplamiento con
  `session-resolver` (ADR-0004).

## More Information

- La propiedad anti-spoof y la tabla de cabeceras inyectadas: sección de seguridad en el edge
  de [`README.md`](../../README.md).
- Related: [ADR-0003](0003-session-state-at-the-edge.md) — la revocación inmediata que exige
  la introspección es su driver dominante.
- Related: [ADR-0004](0004-plugin-chain-order.md) — el orden invertido de la cadena y el
  acoplamiento de los dos plugins de auth.
- Related: ADR-0006 (autorización por app en el edge, pendiente de escribir) — su diseño
  depende de la puerta de roles por ruta que esta decisión conserva.
