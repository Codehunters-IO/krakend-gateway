---
status: proposed
date: 2026-10-07
decision-makers: [Carlos Andres Montoya Tobon]
consulted: [equipo plataforma, seguridad]
informed: [equipo frontend, equipo forgeos, auth-bff]
supersedes: 0002
---

# ADR-0005: Un realm de aplicaciones (`codehunters`) y `master` como realm de operadores

## Context and Problem Statement

Tres ADR aceptados en tres repositorios deciden cosas distintas sobre la misma pregunta —cuántos
realms tiene la plataforma y qué separa cada uno—, y la configuración viva no implementa ninguno
de ellos del todo:

| Repositorio | ADR | Qué decide | Estado |
|---|---|---|---|
| `krakend-gateway` | [0002](0002-platform-edge-global-keycloak-realm-per-product.md) | Keycloak global, **un realm por producto** | accepted 2026-10-06 |
| `react-shell-launcher` | 0001 | **Un realm es un tenant**; las aplicaciones son clientes, no realms | accepted 2026-09-03 |
| `forgeos` | 0006 | La tenencia vive en la aplicación; el edge solo prueba identidad | accepted 2026-08-14 |

Los dos primeros son incompatibles por construcción: si cada producto es un realm, entonces
"un login, varias aplicaciones" es imposible, porque **los realms de Keycloak no comparten sesión
SSO**. El ADR-0001 de la shell lo argumenta en esos términos exactos y es la razón por la que ese
producto registró `shell-web` como cliente en vez de crear un realm.

Medido el 2026-10-07, el estado real difiere de los tres textos:

* **El edge es de un solo realm.** `config/settings/jwt.json` declara un `issuer` y un `jwks_url`,
  ambos `…/realms/forgeos`. La ligadura ruta→realm que la enmienda de 2026-09-29 describe no está
  en la configuración. Realm-por-producto está **decidido pero no construido**.
* **Hay tres instancias de Keycloak en juego y ninguna es "la centralizada"**: `:8081` del compose
  de ForgeOS (realm `forgeos`, imagen **25.0**), `:8082` de `react-shell-launcher` (realm
  `codehunters`, 26.2.4, en `develop` desde hoy) y `:8083`, que es lo que este gateway espera como
  issuer y que no levanta ningún compose del repositorio.
* **El realm `forgeos` no tiene organizaciones.** Son 72 líneas: un cliente, dos roles de realm,
  cero usuarios, `organizationsEnabled` ausente. Las organizaciones de ForgeOS viven en su propio
  Postgres (`OrganizationMember(organizationId, userId, role)`).

A esto se añade una decisión de producto tomada el 2026-10-07: **`auth-bff` pasa a ser la interfaz
de administración de Keycloak** —realms, organizaciones, usuarios, roles— según el usuario y su rol.
Eso obliga a decir de dónde sacan identidad los operadores, pregunta que ningún ADR anterior
responde.

## Decision Drivers

- **"Un login, varias aplicaciones" es el requisito que paga la plataforma.** Es la razón de existir
  de `react-shell-launcher`. Realms separados lo hacen imposible, no difícil: la cookie de sesión de
  Keycloak es por realm, así que el segundo producto siempre pediría contraseña otra vez y mantendría
  una cuenta distinta para la misma persona.
- **Administrar varios realms no requiere que las aplicaciones estén en varios realms.** Son ejes
  distintos y confundirlos es lo que produjo la contradicción.
- **Keycloak ya modela la delegación entre realms, y está verificado.** `master` contiene un cliente
  por realm gestionado —`codehunters-realm`, `master-realm`— con 18 roles finos (`view-users`,
  `manage-users`, `manage-realm`, `impersonation`, …). Un token de `master` respondió **200** contra
  `/admin/realms/codehunters/organizations` en la instancia de desarrollo.
- **Organizations exige Keycloak 26.** Fue preview en 25 —la versión que corre ForgeOS— y soportado
  desde 26. Cualquier diseño que apoye la tenencia en Organizations obliga a subir.
- **Reversibilidad barata, ahora y no después.** Los realms actuales no tienen usuarios reales:
  `forgeos` declara cero y `codehunters` solo el usuario de desarrollo. Borrar y volver a registrar
  cuesta minutos hoy y una ventana de migración dentro de seis meses.

## Considered Options

- **Opción A — Un realm de aplicaciones (`codehunters`) y `master` para operadores.**
- **Opción B — Mantener realm-por-producto** (ADR-0002) y aceptar que no hay SSO entre productos.
- **Opción C — Realm por cliente final** (un realm por cada organización cliente).

## Decision Outcome

Elegida: **Opción A**, que **sustituye a ADR-0002**.

* **`codehunters` es el único realm de aplicaciones.** Cada aplicación es un **cliente** dentro de
  él: `shell-web`, `forgeos-web`, y los que vengan. El realm `forgeos` desaparece y su cliente se
  registra en `codehunters`.
* **`master` es el realm de operadores.** Los administradores de plataforma se autentican ahí, y lo
  que cada uno puede hacer sobre cada realm lo expresan los roles del cliente `<realm>-realm`. El
  modelo de autorización del admin es el nativo de Keycloak; **no se construye uno propio**.
* **`auth-bff` reenvía el token del propio administrador** a la Admin API. No porta credencial
  privilegiada propia: no hay service account omnipotente que robar, y `GET /admin/realms` devuelve
  ya filtrado lo que ese operador puede ver.
* **`impersonation` no se concede nunca.** Está entre los roles delegables; concederlo rompe el
  significado de toda la auditoría posterior, porque una acción «de» un usuario deja de probar que
  ese usuario la hizo.
* **Las organizaciones pasan a ser Keycloak Organizations**, con el **alias** como clave, y el claim
  canónico es `organization`. ForgeOS deja de ser dueño del registro de membresía.
* **El edge vuelve a un solo issuer.** La ligadura ruta→realm de la enmienda de 2026-09-29 deja de
  ser necesaria para los productos; se conserva solo si en el futuro el edge tiene que aceptar
  además tokens de `master`.

### Lo que NO cambia, y es importante

El ADR-0006 de ForgeOS decidió que **el backend es dueño de la selección y la autorización de
tenant**: el cliente manda un selector (`X-Organization-Id`) y el backend lo re-verifica contra la
membresía. Esa decisión **sigue vigente y no se toca**. Lo que cambia es únicamente **de dónde lee
la membresía** ese re-chequeo: de la tabla local a la lista de organizaciones del token.

Esto importa porque el ADR-0006 rechazó una «Opción B — move org switching into the IdP» cuyo
argumento era que *«a single-valued IdP user attribute cannot express "the org this user picked
three seconds ago"»*. Ese argumento es correcto y **no aplica aquí**: Keycloak Organizations no es
un atributo de valor único, es membresía múltiple, y el claim llega como arreglo —medido:
`['globex', 'acme']`—. La selección en caliente se conserva exactamente igual, porque la sigue
haciendo el cliente y verificándola el backend.

El otro driver de aquel ADR, **el arranque en frío (ONB-01)**, sobrevive como restricción: un
usuario nuevo no pertenece a ninguna organización, no lleva claim, y no puede quedar bloqueado
fuera de la llamada que crearía su primera organización. Bajo este ADR, crear la primera
organización es una escritura contra la Admin API, y la ruta que la expone debe tolerar un token
sin claim de organización.

## Consequences

**Buenas**

- SSO real entre la shell y ForgeOS: una contraseña, una cuenta, una sesión.
- El admin multi-realm se apoya en un modelo que Keycloak ya mantiene y audita.
- `required_claims` del edge puede volver a `["sub"]`, alineando la configuración de plataforma con
  la que ForgeOS ya vendoriza, y terminando la divergencia entre ambas.
- Desaparece una de las tres instancias de Keycloak.

**Malas, o que cuestan**

- **El realm `forgeos` se borra y se vuelve a registrar.** Es barato hoy porque no tiene usuarios;
  deja de serlo en cuanto los tenga.
- **ForgeOS deja de ser dueño de la membresía**, y su `TenantMembershipCheck` cambia de fuente de
  datos. El agregado `OrganizationMember` se retira o queda como proyección por alias.
- **Hay que subir Keycloak a 26.x.** Y hay una cadena: `keycloak-js` no existe más allá de 26.2.4,
  mientras que el CVE-2026-18963 pide ≥26.7.2. La cadena se rompe migrando la shell a BFF, que borra
  `keycloak-js` y con él el techo de versión. **Esto fija el orden: BFF antes que la subida.**
- **`auth-bff` pasa a ser multi-issuer**: sesiones de `master` para operadores y de `codehunters`
  para usuarios, sin que una pueda pasar por la otra.
- Un ADR aceptado ayer queda sustituido en menos de 24 horas. Se registra así, sin suavizarlo: la
  contradicción con el ADR-0001 de la shell existía desde el 2026-09-03 y nadie la había cruzado.

## Confirmation

- **Test de integración en el edge**: un token emitido por un issuer distinto del realm de
  plataforma recibe 401. Cubre que no quedó un segundo issuer aceptado por descuido.
- **Test de integración en `auth-bff`**: una petición a `/admin/**` con una sesión de `codehunters`
  —no de `master`— recibe 403 aunque el usuario tenga roles de administrador en su propio realm.
- **Aserción de configuración en CI**: `config/settings/jwt.json` declara exactamente un `issuer`, y
  su realm es el de plataforma. Falla el build si aparece un segundo.
- **Gate de despliegue**: la lista de clientes del realm `codehunters` incluye `forgeos-web`, y el
  realm `forgeos` no existe. Verificable contra la Admin API.
- **Aserción sobre `impersonation`**: ningún usuario ni grupo de `master` tiene ese rol del cliente
  `<realm>-realm`. Comprobable en el mismo gate.

## More Information

- Sustituye a [ADR-0002](0002-platform-edge-global-keycloak-realm-per-product.md) y a su enmienda de
  2026-09-29 (`docs/superpowers/specs/2026-09-29-multi-realm-jwt-design.md`), que deja de ser
  necesaria para separar productos.
- Complementa `react-shell-launcher/docs/adr/0001`, cuya tesis —un realm, un cliente por
  aplicación— este ADR extiende al resto de la plataforma. Ese ADR describe además un modelo de
  autenticación SPA con `keycloak-js` que la migración a BFF deja obsoleto; corresponde a ese
  repositorio registrarlo.
- Enmienda `forgeos/docs/adr/0006` solo en la fuente de la membresía. La propiedad del backend sobre
  la selección y la autorización de tenant se mantiene intacta.
- Evidencia medida el 2026-10-07 en la instancia de desarrollo: cliente `codehunters-realm` presente
  en `master` con 18 roles; token de `master` aceptado con 200 contra
  `/admin/realms/codehunters/organizations`; claim `organization` emitido como arreglo y solo bajo
  el scope `organization:*`.
