FROM --platform=linux/amd64 krakend/builder:2.13.4 AS plugin-builder

COPY plugins/jwt-headers/ /app/plugins/jwt-headers/
WORKDIR /app/plugins/jwt-headers
RUN go build -buildmode=plugin -o /app/jwt-headers.so .

COPY plugins/ip-resolver/ /app/plugins/ip-resolver/
WORKDIR /app/plugins/ip-resolver
RUN go build -buildmode=plugin -o /app/ip-resolver.so .

COPY plugins/trace-context/ /app/plugins/trace-context/
WORKDIR /app/plugins/trace-context
RUN go build -buildmode=plugin -o /app/trace-context.so .

COPY plugins/gateway-timeout/ /app/plugins/gateway-timeout/
WORKDIR /app/plugins/gateway-timeout
RUN go build -buildmode=plugin -o /app/gateway-timeout.so .

COPY plugins/accept-language/ /app/plugins/accept-language/
WORKDIR /app/plugins/accept-language
RUN go build -buildmode=plugin -o /app/accept-language.so .

COPY plugins/session-resolver/ /app/plugins/session-resolver/
WORKDIR /app/plugins/session-resolver
RUN go build -buildmode=plugin -o /app/session-resolver.so .

FROM krakend:2.13.4

COPY --from=plugin-builder /app/jwt-headers.so /opt/krakend/plugins/jwt-headers.so
COPY --from=plugin-builder /app/ip-resolver.so /opt/krakend/plugins/ip-resolver.so
COPY --from=plugin-builder /app/trace-context.so /opt/krakend/plugins/trace-context.so
COPY --from=plugin-builder /app/gateway-timeout.so /opt/krakend/plugins/gateway-timeout.so
COPY --from=plugin-builder /app/accept-language.so /opt/krakend/plugins/accept-language.so
COPY --from=plugin-builder /app/session-resolver.so /opt/krakend/plugins/session-resolver.so

# The image ships the TEMPLATE and its settings, not a pre-rendered
# krakend.json, and renders at container start. An earlier revision rendered
# the config in a build stage and copied only the resulting JSON: that froze
# every `env "..."` lookup to whatever the *builder* had, which was nothing.
# `internal_secret` and `valkey_password` came out as "" — and an empty
# internal_secret makes the session-resolver plugin reject its own config,
# fail to register, and leave the gateway serving without it, 401ing every
# cookie-only request. Runtime rendering is also the only way the Valkey
# password, the CORS/session origins and the backend hosts can differ per
# environment from one image. Baking the secret in with --build-arg would
# have "fixed" the first symptom by writing a live credential into an image
# layer, which is strictly worse.
COPY config/ /etc/krakend/

# Fail the build on a broken template or settings file, exactly as
# `make check` does locally. This renders with no env set, which is fine:
# the check validates structure, and the values that must come from the
# environment are supplied at run time.
RUN FC_ENABLE=1 \
    FC_SETTINGS="/etc/krakend/settings" \
    krakend check -d -t -c "/etc/krakend/krakend.tmpl"

ENV FC_ENABLE=1 \
    FC_SETTINGS=/etc/krakend/settings

EXPOSE 8080 8443

# Required at run time (no defaults, by design): INTERNAL_SHARED_SECRET and,
# wherever Valkey is password-protected, VALKEY_PASSWORD.
CMD ["run", "-c", "/etc/krakend/krakend.tmpl"]
