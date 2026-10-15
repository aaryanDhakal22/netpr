# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/netpr ./cmd/netpr

# Alpine rather than distroless: printers that can't print PDF get rendered
# page images, made with poppler's pdftoppm. Liberation fonts stand in for
# Helvetica/Arial/Times in PDFs that don't embed their fonts.
FROM alpine:3.21
RUN apk add --no-cache poppler-utils font-liberation ca-certificates \
 && adduser -D -H -u 65532 nonroot \
 && mkdir /data && chown nonroot:nonroot /data
COPY --from=build /out/netpr /netpr
USER nonroot:nonroot
ENV NETPR_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/netpr", "healthcheck"]
ENTRYPOINT ["/netpr"]
