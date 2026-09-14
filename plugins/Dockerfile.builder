FROM golang:1.25.9-alpine

RUN apk add --no-cache build-base binutils-gold

WORKDIR /app
