# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY *.go loading.html ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /quantixy .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /quantixy /quantixy
COPY services.yaml /etc/quantixy/services.yaml
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["/quantixy", "-health"]
ENTRYPOINT ["/quantixy"]
