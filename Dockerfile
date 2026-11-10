# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/icinga-kubernetes-notify ./cmd/icinga-kubernetes-notify

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/icinga-kubernetes-notify /icinga-kubernetes-notify
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/icinga-kubernetes-notify"]
CMD ["--config", "/etc/icinga-kubernetes-notify/config.yml"]
