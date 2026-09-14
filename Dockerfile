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

FROM krakend:2.13.4 AS builder

COPY --from=plugin-builder /app/jwt-headers.so /opt/krakend/plugins/jwt-headers.so
COPY --from=plugin-builder /app/ip-resolver.so /opt/krakend/plugins/ip-resolver.so
COPY --from=plugin-builder /app/trace-context.so /opt/krakend/plugins/trace-context.so
COPY --from=plugin-builder /app/gateway-timeout.so /opt/krakend/plugins/gateway-timeout.so
COPY --from=plugin-builder /app/accept-language.so /opt/krakend/plugins/accept-language.so
COPY --from=plugin-builder /app/session-resolver.so /opt/krakend/plugins/session-resolver.so
COPY config/ /etc/krakend/

ARG ENV=dev

RUN FC_ENABLE=1 \
    FC_SETTINGS="/etc/krakend/settings" \
    FC_OUT="/tmp/krakend.json" \
    krakend check -d -t -c "/etc/krakend/krakend.tmpl"

FROM krakend:2.13.4

COPY --from=plugin-builder /app/jwt-headers.so /opt/krakend/plugins/jwt-headers.so
COPY --from=plugin-builder /app/ip-resolver.so /opt/krakend/plugins/ip-resolver.so
COPY --from=plugin-builder /app/trace-context.so /opt/krakend/plugins/trace-context.so
COPY --from=plugin-builder /app/gateway-timeout.so /opt/krakend/plugins/gateway-timeout.so
COPY --from=plugin-builder /app/accept-language.so /opt/krakend/plugins/accept-language.so
COPY --from=plugin-builder /app/session-resolver.so /opt/krakend/plugins/session-resolver.so
COPY --from=builder /tmp/krakend.json /etc/krakend/krakend.json
RUN chmod 644 /etc/krakend/krakend.json

EXPOSE 8080 8443

CMD ["run", "-c", "/etc/krakend/krakend.json"]
