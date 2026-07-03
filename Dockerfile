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

FROM krakend:2.13.4 AS builder

COPY --from=plugin-builder /app/jwt-headers.so /opt/krakend/plugins/jwt-headers.so
COPY --from=plugin-builder /app/ip-resolver.so /opt/krakend/plugins/ip-resolver.so
COPY --from=plugin-builder /app/trace-context.so /opt/krakend/plugins/trace-context.so
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
COPY --from=builder /tmp/krakend.json /etc/krakend/krakend.json
RUN chmod 644 /etc/krakend/krakend.json

EXPOSE 8080 8443

CMD ["run", "-c", "/etc/krakend/krakend.json"]
