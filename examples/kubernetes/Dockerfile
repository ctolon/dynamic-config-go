# The example service, as a container, for the end-to-end Kubernetes test.
#
# Built from the repository root:
#
#   docker build -f examples/kubernetes/Dockerfile -t dynamic-config-go-example:e2e .
FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies first, so that editing the library does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static, so the final image needs no libc and no shell.
ENV CGO_ENABLED=0

RUN go build -trimpath -ldflags="-s -w" -o /out/example ./examples/kubernetes

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/example /example

USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/example"]
