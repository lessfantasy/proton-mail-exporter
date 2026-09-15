FROM golang:1.27-alpine AS builder

WORKDIR /src

COPY go.mod main.go ./
RUN go mod tidy

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /proton-mail-exporter .


FROM alpine:3.22

RUN apk add --no-cache ca-certificates && \
    addgroup -S exporter && \
    adduser -S -G exporter exporter

COPY --from=builder /proton-mail-exporter /usr/local/bin/proton-mail-exporter

USER exporter

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/proton-mail-exporter"]
