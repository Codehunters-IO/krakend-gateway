---
status: accepted
date: 2026-10-06
accepted-date: 2026-10-07
decision-makers: [Carlos Andres Montoya Tobon]
consulted: [equipo plataforma]
informed: [equipo forgeos]
---

# ADR-0004: El array `plugin/http-server` se declara invertido, y los dos plugins de auth van juntos

Formato Nygard (*Documenting Architecture Decisions*, 2011): decisión obligada, consecuencias
simples. Se conserva el frontmatter que usa el resto del registro para que el índice siga
funcionando, y se añade `Confirmation`, que Nygard no contempla pero el registro exige.

## Context

KrakenD ejecuta el array `extra_config."plugin/http-server".name` **al revés de como se
declara**: cada entrada envuelve a las anteriores, así que para `name: [A, B, C]` la composición
es `C(B(A(router)))`. C, declarado último, ve la petición primero. A, declarado primero, se
ejecuta último, pegado al router.

Esa regla ya rompió el edge una vez. El array se declaró en orden de cadena —lo intuitivo— y eso
hizo que `jwt-headers` se ejecutara **primero**, rechazando con `401` toda petición que solo
traía cookie de sesión antes de que `session-resolver` llegara a convertirla en `Authorization:
Bearer`. El patrón Token Handler (ADR-0003) quedó inoperativo para el navegador.

Lo que hace esto peligroso es que fue invisible a todo lo que se mira por defecto:
`krakend check` seguía respondiendo «Syntax OK!», el JSON renderizado leído a ojo parecía
correcto —de hecho parecía *más* correcto, porque estaba en el orden que uno espera—, y los seis
plugins seguían emitiendo «plugin loaded». El fallo solo se ve ejerciendo una petición real con
cookie y sin `Bearer`.

Hay una segunda propiedad, independiente del orden y de peor consecuencia:
**`session-resolver` habilitado sin `jwt-headers` es un edge fail-open.** Su rama
`actionPassThrough` reenvía las peticiones que no traen ni cookie ni `Authorization`, apoyándose
en que `jwt-headers` las rechazará después. Sin `jwt-headers` en la cadena no las rechaza nadie:
toda petición no autenticada llega al backend. Los dos plugins están acoplados, y un feature-flag
apagado «para probar» basta para abrir el edge.

## Decision

El array `plugin/http-server.name` de `config/krakend.tmpl` se declara **en orden inverso a la
cadena de ejecución**: `krakend-jwt-headers` primero, de modo que se ejecute último y más cerca
del router, y `krakend-session-resolver` inmediatamente después, de modo que se ejecute
inmediatamente antes.

El orden de ejecución resultante, de primero a último en ver la petición:

```
gateway-timeout → accept-language → trace-context → session-resolver → jwt-headers
```

Y `session-resolver` y `jwt-headers` se habilitan o se deshabilitan **juntos**. La combinación
«sesión sí, JWT no» no debe renderizar nunca.

Quien añada un plugin a ese array coloca su entrada razonando al revés: la posición en el JSON
es la inversa del momento en que quiere que corra.

## Consequences

**Más fácil.** El orden queda verificable sin arrancar el gateway ni ejercer tráfico: se
comprueba sobre el JSON renderizado. Un flag mal puesto que abriría el edge falla en CI en vez
de en producción.

**Más difícil.** El array del template lee al contrario de la cadena, lo que es antiintuitivo
para cualquiera que lo abra por primera vez. Se mitiga con el comentario extenso en
`config/krakend.tmpl` y con la sección de orden de cadena de
[`docs/session-flow.md`](../session-flow.md), que documenta cómo se encontró y confirmó. Tocar
ese array exige leer una de las dos cosas primero.

**Requisito nuevo que cae de la decisión.** Añadir un plugin al array no es editar una lista: es
elegir una posición en una composición invertida. Y si el plugin nuevo depende de otro —como
`session-resolver` depende de `jwt-headers`— la dependencia debe quedar guardada, no solo
documentada.

## Confirmation

`scripts/check-plugin-chain-order.sh`, 104 líneas, dentro de `make check` y por tanto en cada
pull request. Renderiza la Flexible Configuration real con `krakend check -d -t`, inspecciona el
array resultante con `jq` y falla con mensaje explícito en tres casos:

1. `krakend-jwt-headers` no es la primera entrada.
2. `krakend-session-resolver` no es la segunda.
3. `session-resolver` está habilitado y `jwt-headers` no — el caso fail-open, que falla en vez de
   leerse como «nada que comprobar».

**Su punto ciego, que hay que conocer.** Con `SESSION_ENABLED=false` el guardia responde
`OK (session-resolver disabled, nothing to check)` y termina con éxito. Es deliberado —sin
`session-resolver` la restricción de adyacencia es irrelevante— pero significa que **el guardia
solo verifica el orden cuando la sesión está habilitada**. Un `make check` corrido con la sesión
apagada no dice nada sobre esta decisión, aunque su salida parezca conforme.

A diferencia de ADR-0001, cuyo smoke test nunca se escribió, y de ADR-0003, que no puede pasar a
`accepted` hasta tener los valores definitivos de los tres relojes, esta confirmación **ya existe
y ya pasa en verde**.

**Estado de la confirmación al aceptar (2026-10-07).** Los tres puntos están cubiertos y
ejercitados, no solo escritos. `make check` corre `scripts/check-plugin-chain-order.sh` en cada
pull request y responde
`OK (krakend-jwt-headers declared first / executes last, krakend-session-resolver immediately
before it)`. El punto ciego descrito arriba sigue siendo real y es la única reserva: con
`SESSION_ENABLED=false` el guardia termina con éxito sin comprobar nada. No se corrige al aceptar
porque la restricción de adyacencia es genuinamente irrelevante en esa configuración; queda
documentado para que una salida conforme no se confunda con una verificación.

## More Information

- Cómo se encontró el fallo de orden y la evidencia A/B que lo confirmó: sección de orden de
  cadena en [`docs/session-flow.md`](../session-flow.md).
- Comentario en el propio array, con la regla de envoltura y la advertencia:
  `config/krakend.tmpl`.
- Related: ADR-0003 (estado de sesión en el edge) — el acoplamiento que esta decisión guarda es
  lo que hace utilizable el patrón que aquel registra.
